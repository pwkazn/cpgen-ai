# 配置模型

## 1. 配置分层

配置分为两类：

- ApplicationConfig：服务地址、存储、Docker、模型供应商和默认策略，由部署者管理。
- GenerationRequest：题目需求、难度、限制、seed、预算和导出目标，由用户为每个 run 提交。

Run 创建时保存去密后的有效配置快照和 digest。后续全局配置变化不能静默改变已有 run；必须通过 `ReviewDecision(REVISE)` 或创建新 run。

## 2. ApplicationConfig 示例

```yaml
schema_version: cpgen.config/v1

orchestrator:
  lease_ttl: 30s
  heartbeat_interval: 5s

storage:
  database: ./var/cpgen.sqlite3
  artifact_root: ./var/artifacts
  work_root: ./var/work
  output_root: ./output
  max_total_bytes: 21474836480

llm:
  provider: openai_compatible
  base_url: https://example.invalid/v1
  api_key_env: CPGEN_LLM_API_KEY
  model: configured-model
  connect_timeout: 5s
  operation_timeout: 180s
  max_response_bytes: 4194304
  retries: 3
  pricing_policy_file: ./config/llm-pricing-v1.json
  pricing_policy_digest: sha256:REQUIRED
  privacy_policy:
    version: llm-data-policy-v1
    policy_digest: sha256:REQUIRED
    endpoint_class: public_remote   # local | private_remote | public_remote
    allow_remote_submission: true  # 远端必须显式授权；默认 false
    allowed_data_classes:
      - user_requirements
      - unpublished_problem
      - generated_source
      - test_plan
      - validator_feedback

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
  health_ttl: 5m
  privacy_class: public
  allow_public_submission: true  # 必须显式确认向第三方发送未发布题面
  failure_policy: blocked

sandbox:
  backend: docker
  engine_endpoint: npipe:////./pipe/docker_engine  # Linux 示例：unix:///var/run/docker.sock
  forbid_ambient_docker_context: true
  builder_image: cpgen-builder@sha256:REQUIRED
  runtime_image: cpgen-runtime@sha256:REQUIRED
  transfer_image: cpgen-transfer@sha256:REQUIRED
  execution_protocol: docker-direct-v2
  compile_profile: compile-v2
  execute_profile: execute-mvp-v2
  log_profile: none-attach-v1
  output_volume_profile: local-tmpfs-keeper-v1
  watchdog:
    executable: self
    ack_timeout: 3s
    safety_slack: 15s
    clean_record_retention: 10m  # 仅 CLEANED 后；armed/UNKNOWN tombstone 不按 TTL 删除
  operation_timeouts:
    create: 10s
    attach_and_arm: 5s
    engine_start: 10s
    stop_grace: 500ms
    kill_and_wait: 10s
    evidence_and_export: 60s
    cleanup: 30s
  compile_limits:
    cpu_count: 2
    memory_bytes: 1073741824
    pids: 64
    output_bytes: 268435456
  execute_limits:
    pids: 32
    stdout_bytes: 67108864
    stderr_bytes: 4194304
    shm_bytes: 16777216
  release:
    require_rootful_local_linux: true
    require_cgroup_v2_parent: true
    serial_resource_gate: true
    cpuset: configured-host-cpuset

policies:
  similarity_version: similarity-policy-v1
  quality_version: quality-policy-v1
  max_package_attempts: 3
```

示例中的模型/镜像值是占位符；启动时必须解析为具体允许值。

## 3. GenerationRequest 示例

```yaml
schema_version: cpgen.request/v1
mode: manual
brief: 生成一道以最短路状态设计为核心的题目
keywords: [路径, 状态]
algorithm_tags: [shortest-path]
language: zh-CN
topic_tags: [graph, shortest-path]
difficulty: medium
required_features: []
forbidden_features: [interactive]
time_limit_ms: 2000
memory_limit_mb: 256
solution_language: cpp20
seed: 20260829
verification_profile: mvp
export_targets: [internal]
budgets:
  max_total_llm_calls: 40
  max_similarity_calls: 20
  max_cost_usd: 10
  max_wall_time: 2h        # 累计 RUNNING 且有效 execution/recovery lease 覆盖的区间
  max_mutations_per_stage: 4
  max_sandbox_runs: 1000
  max_artifact_bytes: 1073741824
  max_package_bytes: 2147483648
```

### 3.1 GenerationRequestV1 字段规则

```text
提交 DTO：
schema_version
mode(manual|random)
brief?, keywords[], algorithm_tags[], topic_tags[]
difficulty, language, required_features[], forbidden_features[]
time_limit_ms, memory_limit_mb, solution_language
verification_profile, seed?, budgets, export_targets[]

持久化 GenerationRequestSnapshotV1：
request_id, request_digest, submitted_request, effective_seed
redacted_effective_config, effective_config_digest, created_at
```

- `manual` 要求 `brief/keywords/algorithm_tags/required_features` 至少一项非空；`random` 允许全部为空，但限制、负向约束和预算仍有效。
- `request_id/request_digest/effective_seed` 不是客户端可提交字段；严格提交 Schema 必须拒绝它们。创建 run 时若未提供 seed，Orchestrator 生成并在同一事务持久化 snapshot/effective seed；同一 snapshot 还内联保存去密后的有效配置（密钥只保留 provider/key-ref，不保存值）及 digest。所有后续 Idea、mutation、Statement、TestPlan 和崩溃恢复只读取该不可变 snapshot，不能回读已变化的全局配置或再次取系统随机数。
- `algorithm_tags/topic_tags` 来自版本化 allowlist；未知标签、超长 brief/tag、重复归一化标签、required/forbidden 冲突和无效资源限制在创建 run 前拒绝。
- `brief` 是用户自由简述；`keywords` 是概念词；`algorithm_tags` 是期望解法；`topic_tags` 是题型/知识域。四者不得被 adapter 偷换为同一个字段，规范化结果和原输入都进入 request digest/provenance。

`max_cost_usd` 只接受可精确转换为整数 micro-USD 的 decimal；内部不使用 float。具体模型费率来自 digest 固定的 `PricingPolicyRef`，不是代码常量或运行时网页价格。

## 4. 优先级

1. CLI 显式 flag。
2. 指定的 ApplicationConfig/GenerationRequest 文件。
3. 项目默认配置。

环境变量只用于密钥和显式标记为 env-overridable 的部署字段。不得用环境变量悄悄覆盖题目语义、budget 或 verification profile。

## 5. 校验

- 所有 Schema 拒绝未知字段，升级通过新 `schema_version`。
- 相对路径相对于配置文件目录解析，再验证位于允许 workspace root 内。
- artifact/work/output/database 路径不得互相嵌套为危险关系；work 可清理但 artifact/output 不可被 work GC 覆盖。
- `output/.staging` 必须由程序固定创建在 `output_root` 内，启动时验证 staging/final 同 volume 及所选 profile 需要的原子 rename/durable flush 能力。
- URL 必须有 scheme/host；禁止内嵌用户名密码。
- 非本地 Similarity endpoint 必须声明 privacy_class；public endpoint 默认禁止提交，只有显式 `allow_public_submission: true` 才允许发送。
- timeout、资源和预算必须为正且受部署上限限制。
- LLM pricing policy 文件/digest 必须一致，币种为请求预算支持的币种，effective model 和所有可能 usage category 均有整数费率；缺失时配置失败，不能发送模型请求。
- LLM endpoint 必须声明版本化且 digest 自校验的 privacy policy。remote 默认 `allow_remote_submission=false`；每个 PromptRef 声明的 data classes 必须是 allowlist 子集，密钥/第三方正文等禁止类别永远不能授权。缺授权可保存配置但 run 在发送前进入 `NEEDS_REVIEW(llm_remote_submission_not_authorized)`，请求数为零。
- `lease_ttl` 必须显著大于 heartbeat interval，且两者受部署最小/最大值约束。
- 持久化 lease epoch 使用 SQLite 可精确保存的正 64 位整数范围，并在接近上限时失败关闭；不得回绕或重置计数器。
- Docker image 参与质量门禁时必须含 digest。
- 所选 Docker profile 必须通过版本化 capability canary：mvp 至少验证 quota tmpfs volume 跨 target stop、keeper/export、none+attach 日志、detached watchdog、`memory.max`/`memory.swap.max=0`、主/子进程 OOM 与输出限制；`release` 还必须通过 Engine/host allowlist、持久父 cgroup 和瞬时进程指标测试。
- Docker endpoint 必须在有效配置中规范化并固定；MVP Schema 只允许本机 `unix://`（Linux）或 `npipe://`（Windows Docker Desktop）端点，明确拒绝 `tcp://`、`ssh://`、remote context 和任何隐式当前 CLI context；release 再叠加批准的 rootful Linux host allowlist。禁止继承 `DOCKER_HOST/DOCKER_CONTEXT`。CapabilitySnapshot/cache/checkpoint、Runner 和 watchdog 都绑定同一 `engine_identity_digest`（endpoint digest、daemon ID、Engine instance/boot marker、server/API/OS/arch、runtime/cgroup/security options 与三类镜像 digest）；每次 dispatch 和 cleanup TAKEOVER 都重新核对，身份变化使旧 capability 立即失效并触发新 run-scoped probe。
- `program_hard_deadline` 来自题目 time limit/Runner profile，只覆盖 Start 明确成功后的目标程序；`step_deadline`、`run_budget_deadline` 和 create/attach/start/cleanup 上限分别配置并在有效配置 snapshot 中固定，禁止复用一个 `deadline` 字段。

## 6. 密钥

- 配置只保存环境变量名，不保存值。
- 日志和错误输出对 Authorization、API key 和 query 参数凭据脱敏。
- Sandbox 环境永不继承密钥。
- provenance 记录 provider/model，但不记录 token。

## 7. 配置变更

- 非语义部署配置（日志级别）可在新请求时生效。
- 端口、模型、prompt、Policy、工具链和 profile 变化产生新的有效配置 digest。
- 对进行中的 run 修改需 `ReviewDecision(REVISE)`，随后按 workflow 依赖图失效相关结果。
- Secret rotation 不改变语义 digest，但必须记录 provider 调用发生时间和 endpoint identity。

## 8. 命令

```text
cpgen config validate --config cpgen.yaml
cpgen config effective --config cpgen.yaml --redact
cpgen doctor --config cpgen.yaml
```

`doctor` 检查 SQLite 路径、Docker 静态 capabilities/镜像 digest、LLM 配置/定价/privacy policy 和 Similarity health，但不产生题目 run、不创建 Docker canary 容器、不发送 prompt/题面；动态 sandbox profile 只展示最近 run-scoped canary 摘要/待验证状态。输出 endpoint class 与允许的数据类别并对远端未授权告警。
