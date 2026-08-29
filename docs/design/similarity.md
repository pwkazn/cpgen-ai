# Similarity Service 与决策策略

## 1. 边界

Similarity adapter 负责调用服务并保存检索证据；Similarity Policy 负责根据证据决定 `PASS/MUTATE/NEEDS_REVIEW`。服务自身的分数不是“原创概率”。

Step 侧与物理调用侧使用分层端口：

```go
type SimilarityAdapter interface {
	Search(ctx context.Context, auth DispatchAuthorization, req SimilaritySearchRequest) (SimilarityAttemptOutcome, error)
	Health(ctx context.Context, auth DispatchAuthorization, req SimilarityHealthRequest) (CapabilityAttemptOutcome, error)
}

type SimilarityAttemptOutcome struct {
	CallID   AttemptCallID
	Response *SimilarityAttemptResponse
	Failure  *PhysicalPortFailure
}

type SimilarityAttemptResponse struct {
	Hits             []SimilarityHit
	Capabilities     Capabilities
	EffectiveOptions SearchOptions
	ModelVersion     string
	IndexVersion     string
	ResponseMetadata map[string]json.RawMessage
	RawBlob          *PendingArtifact
}
```

`SimilarityAttemptOutcome` 的 Response/Failure 恰有一个并始终回显 CallID；HTTP/协议/服务拒绝属于 typed `PhysicalPortFailure`，普通 error 只用于 context/内部错误。`SimilarityAttemptResponse` 只包含适配器归一化的 hits/effective options、服务能力/版本、受限 response metadata 和已授权 raw-response PendingArtifact；它不是可提交的 `SimilarityEvidence`。工作流 Step 只调用 `MeteredSimilarity`。每次实际的搜索、健康检查及其物理重试都先认领新的 `AttemptCall`，再由计量代理向底层适配器传入不可伪造的 `DispatchAuthorization`；Metered proxy 核对每次回显并把成功或失败的物理重试、缓存/熔断来源组装为公共 `MeteredOutcome/CallTrace`，全部失败也不会丢失 provenance。

## 2. 类型

```go
type SimilarityEvidence struct {
	Provider          string
	ServiceIdentity   string
	QueryDigest       Digest
	Hits              []SimilarityHit
	Capabilities      Capabilities
	EffectiveOptions  SearchOptions
	ModelVersion      string
	IndexVersion      string
	ResponseMetadata  map[string]json.RawMessage
	RetrievedAt       time.Time
	CallTrace         CallTrace
}

type SimilarityDecision struct {
	EvidenceDigest Digest
	PolicyVersion  string
	Decision       string // PASS | MUTATE | NEEDS_REVIEW
	ReasonCodes    []string
	ConfidenceBand string
}
```

Evidence 不包含 threshold 或最终 decision。

## 3. 配置

```yaml
similarity:
  base_url: https://yuantiji.ac
  protocol: yuantiji_v2
  search_path: /api/search
  health_path: /api/health
  connect_timeout: 5s
  response_header_timeout: 15s
  operation_timeout: 120s
  max_request_bytes: 65536
  max_response_bytes: 4194304
  follow_redirects: false
  auth_token_env: ""
  privacy_class: public
  allow_public_submission: true
  retries: 3
  breaker_failures: 5
  health_ttl: 5m
  unversioned_cache_ttl: 10m
```

`protocol` 提供默认 path 和 wire mapping，显式 path 覆盖默认值。`base_url` 只选择部署地址。Service identity 是去除凭据后的规范化 scheme/host/port/base-path 加 protocol，不包含 token。

## 4. yuantiji_v2 adapter

请求：`POST /api/search`。

```json
{
  "query": "...",
  "k": 50,
  "rewrite": true,
  "rerank": true,
  "skip_short": true,
  "sources": null
}
```

响应字段 `results[].uid/title/src/url/cos/rr/base_rank/original/t0/t1` 在 adapter 中映射为内部 hit。业务层不得读取供应商字段。Adapter 记录服务实际是否执行 rewrite/rerank；服务静默降级时必须反映在 `EffectiveOptions/Capabilities`。

Health 响应用于能力和模型信息，但不得假设服务总能提供稳定 index version。Problem count 只能作为弱版本线索，不能单独作为强缓存版本。

## 5. HTTP 安全

- 默认只允许 HTTPS；`http://localhost` 和显式开发 allowlist 例外。
- 禁止跨 host redirect；MVP 默认完全禁止 redirect。
- auth token 从命名环境变量读取，不写入配置快照、日志或 cache key。
- 请求前检查字节上限，响应使用 `io.LimitReader` 并拒绝超限。
- 需保留的响应证据只通过 run-scoped `MeteredArtifactSink` 写 Blob；adapter 不直接创建 occurrence 或使用裸 ArtifactStore 写入口。
- 只接受 JSON Content-Type 和受支持 Schema；未知必需字段/类型变化失败关闭。
- 日志保存 query digest、长度、选项和 timing，不默认保存未发布完整题面。

## 6. 重试与熔断

- connect reset、429、502/503/504 可重试；遵守 `Retry-After` 并使用指数退避+jitter。
- 4xx Schema/认证错误不盲重试。
- operation timeout 覆盖包含重试的整个逻辑调用；connect/header 等 timeout 限制每个物理请求，外层 `step_deadline/run_budget_deadline` 仍可更短。
- 每个物理 HTTP attempt 必须先经 Metered proxy 单独预留 Similarity call；adapter 不得在计量代理之下静默重试。
- 熔断器按 service identity 维护；打开后直接返回 `Blocked(dependency=similarity_service)`。
- 响应 Schema 不兼容是配置/协议错误，进入 `FAILED` 或需运维修复，不当作低相似度。

## 7. Evidence cache

强版本流程：

1. 获取未过期的 health capability/version snapshot；超过 `health_ttl` 必须条件重验证或重新请求，不能无限使用旧 snapshot。
2. 使用 query digest、service identity、protocol、请求选项、model/index version 计算 key。
3. 命中则返回带缓存 provenance 的 `PendingArtifact/SimilarityEvidence`；`CallTrace.dispatch_kind=CACHE_HIT`，记录原 provider call 与当前 CACHE_PIN，不伪造本次 HTTP dispatch；由 Orchestrator 在 attempt 提交事务中创建新的 ArtifactOccurrence。
4. 请求后若实际选项或版本与预期不同，使用实际值重新计算 key 再保存。

无稳定版本：

- key 包含 service identity、query 和实际选项。
- 使用短 TTL。
- health 模型/问题数量变化时主动失效该服务的无版本缓存。
- 不允许永久保存为原创性结论。

health 重验证失败时：已经作为当前 run 制品提交且仍匹配输入 revision 的 Evidence 可以继续参与决策；尚未提交的新 Similarity step 不得仅凭过期 health/cache 产生新 Evidence，任务进入 `BLOCKED`。恢复时先按 workflow 进入 `RUNNING(mode=PROBING)`，再以正常 AttemptCall 和 `max_similarity_calls` 预算执行 health 请求；失败收敛后回到 BLOCKED。这样恢复已有证据与为新题目查询服务的风险边界不同。

CapabilitySnapshot cache hit 同样使用 `CallTrace(dispatch_kind=CACHE_HIT)`：来源 snapshot/call 与当前 PROBE_CACHE_PIN 分列记录，`result_attempt_call_id` 为空，不能把来源 call 冒充本次物理探测。用于 BLOCKED resume 时还必须引用同 service identity/policy 的 `capability_observations`，且 HEALTHY observation sequence 达到 checkpoint floor。真实 retry 的所有 ID 进入 `physical_attempt_call_ids`，最终 adapter 回显 ID 必须等于 `result_attempt_call_id`。

## 8. Decision cache 与 Policy

Decision cache key：`sha256(evidence_digest + policy_version)`。

Policy 输入包括：

- Top-K cosine/rerank 特征。
- 是否成功 rewrite/rerank。
- 重复/同源聚合信息。
- 题目抽象算法、约束和操作结构的可解释比较（如有）。
- 已校准阈值版本。

输出规则：

- 高风险：`MUTATE`，Evidence 进入 Idea/Statement 变异提示，但不得复制第三方题面。
- 灰区或服务能力降级：`NEEDS_REVIEW`。
- 低风险且证据完整：`PASS`。
- 服务不可用：不是 decision，任务 `BLOCKED`。

阈值只能通过标注集校准后版本化发布。修改 Policy 只重算 Decision，不重复请求 Evidence。

## 9. 隐私与保留

- 第三方服务会看到待查题面。非本地服务必须配置 `privacy_class`；当值为 `public` 时，只有显式 `allow_public_submission: true` 才能发送。默认 false，缺少授权时任务进入 `NEEDS_REVIEW(public_submission_not_authorized)`，`--json`/CI 不弹交互确认也不发送数据。
- 可配置只发送去故事化的结构摘要，但摘要算法版本必须进入 query digest/provenance。
- 本地日志默认不保留完整请求；完整题面只保存在本地受控 Blob。
- Similarity hits 仅作证据，题包不得打包第三方完整题面。

### 9.1 Package-safe 投影

包内 `reports/similarity.json` 只能由独立的 `cpgen.similarity-report/v1` DTO 序列化，禁止直接 marshal `SimilarityEvidence` 或供应商响应：

```text
SimilarityPackageReport
  schema_version, query_digest, local_evidence_digest, decision_digest
  provider_label, protocol, decision, policy_version
  reason_codes[], confidence_band
  hits[]
    problem_id, title, source, canonical_url
    base_rank, normalized_similarity?, normalized_rerank?
```

- `title/source/problem_id` 有严格长度和字符上限；URL 仅允许 http/https，移除 userinfo、fragment 和敏感/非 allowlist query 参数。
- Schema 没有 `original/t0/t1/snippet/body/raw_response/response_metadata/full_query` 字段，也拒绝未知字段。第三方正文和 raw provider metadata 只可存在于本地受控 Evidence Blob，不进入包、LLM prompt 或普通日志。
- `local_evidence_digest` 只作为审计指针；题包不要求携带该本地私有 Blob。`decision_digest` 指向由该 Evidence 和固定 Policy 产生的 `SimilarityDecision`。
- generation/derived Package Gate 必须从 current snapshot/PrePackage provenance 取得实际的 `SimilarityEvidence` 与 `SimilarityDecision` occurrence，按 v1 规则确定性重建 safe DTO，并逐项核对 `query_digest/local_evidence_digest/decision_digest`、decision、policy version、reason codes、confidence band 及安全 hit 投影；仅通过 Schema 不算通过。
- 外部包的 StructuralGate 只能检查包内 report、origin PrePackage/provenance 与 manifest 的 digest 交叉引用自洽，并明确标记为 origin attestation，不能声称本地私有 Evidence 仍存在或仍然时效有效。外部 verification run 必须取得新的 current Evidence/Decision、独立评估，并把新 digests 绑定到包外 current PrePackage evidence、receipt 和最终 QualityReport；origin 投影不能冒充 current 查重证据。
- 变异/审核需要解释时使用 reason code、短标题和结构化差异特征；若未来确需第三方片段，必须新增版权/隐私 ADR 和新的显式 Schema，不能扩展 v1 未知字段。

## 10. 测试

- Fake service：正常、rewrite 降级、rerank 降级、429、timeout、5xx、超大响应、错误 Content-Type、Schema 漂移。
- base URL 改变只改变 service identity，不改变业务代码。
- Policy 版本变化只使 Decision cache miss。
- 模型/index 版本变化使 Evidence cache miss。
- 无版本服务 TTL 到期后必须重新请求。
- health TTL 到期后索引升级能切换 Evidence cache key；重验证不可达时新查询进入 BLOCKED。
- 熔断后任务进入 `BLOCKED`，恢复后从原 Similarity step 继续。
- public 服务未显式授权时请求数为零，并返回可审核的拒绝证据。
- 将含 `original/t0/t1/snippet/raw metadata` 的 Evidence 投影到题包时，这些字段不可表示；未知字段、带凭据 URL 和超长标题被拒绝。
- generation/derived 构建时篡改 package report 的 Evidence/Decision digest、policy、reason 或 hit 投影，Package Gate 均拒绝；只满足 v1 Schema 不能通过。
- 外部验证将 origin report 标为历史 attestation，并用新 Evidence/Decision 产生包外 current 门禁；服务不可达时不能复用 origin PASS 进入 READY。
