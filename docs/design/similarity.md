# 查重服务与决策策略

状态：ADR-0006 下为当前设计

## 1. 边界

查重是通过 MeteredSimilarity 进行的普通类型化阶段本地调用。它比较规范的题包安全投影并返回证据；由应用策略而非远程服务决定接受、拒绝、复核或阻塞。

该适配器不能变更 run 状态、发布制品或选择 workflow 阶段。

## 2. 类型

~~~text
SimilarityRequest
  schema_version
  candidate_projection_digest
  normalized_title
  normalized_statement
  normalized_tags
  language
  policy_ref
  policy_digest
  logical_idempotency_key

SimilarityHit
  source
  external_id
  score
  canonical_url?
  title_digest?
  evidence_digest

SimilarityEvidence
  request_digest
  provider_identity
  hits
  score_summary
  observed_at
  policy_digest
  CallTrace

SimilarityDecision
  ACCEPT | REJECT | NEEDS_REVIEW | BLOCKED
  rule_id
  evidence_digest
  explanation_code
~~~

不发送原始私有生成上下文。只有显式定义的题包安全投影才有资格被发送。

## 3. 配置与策略

配置定义端点身份、HTTPS 与主机允许列表、按环境引用提供的凭据、超时、最大响应字节数、最大命中数、重试上限、缓存 TTL、分数阈值、复核区间和策略 revision。

阈值语义是显式的，并在精确边界上测试。提供方分数是有限的规范化小数。未知算法或缺失策略元数据时失败关闭。

## 4. 适配器行为

适配器：

1. 规范化并哈希请求；
2. 预留调用和成本预算；
3. 记录稳定逻辑身份和物理调用；
4. 应用 DNS/IP/HTTPS、重定向、超时、解压缩和响应大小规则；
5. 严格解码完整响应；
6. 确定性地规范化并排序命中；
7. 结算预算并存储 CallTrace；
8. 向阶段策略返回类型化证据。

提供方特定的响应字段保留在适配器内部。URL 在展示或题包安全投影前会被净化。

## 5. 重试与阻塞

瞬态连接、超时、限流和特定服务器错误可在前台尝试期间使用有界重试。永久性的认证、策略、schema 或兼容性错误按类型化规则阻塞或失败。

未知发送边界时，若可用则使用原始幂等身份与提供方核对；否则记账从保守处理，阶段进入类型化暂停或复核路径。

当依赖不可用时，BLOCKED 记录确切的端点身份、策略 digest、请求输入 digest、错误证据和重试等待时间。

## 6. 人工恢复

人工恢复获取 run 进程锁，并为 Similarity 阶段创建一次全新尝试。在查询新证据之前，该尝试在当前策略和预算下通过 MeteredSimilarity 重新校验检查点依赖。

历史健康或能力数据本身不能授权新证据。如果校验失败，新尝试返回 BLOCKED。已为同一输入 revision 提交的既有证据仍可审计，并且只有在策略明确允许时才可被考虑。

## 7. 证据缓存

缓存键包括适配器协议、端点身份、规范候选投影、语言、算法 revision 和策略 digest。

有效命中必须保留：

- 原始来源 CallTrace；
- 提供方身份和观测时间；
- 完整的规范化命中与分数摘要；
- 策略和请求 digest；
- 当前 run 的缓存使用与制品来源。

过期、策略不匹配、Blob 证据损坏或更新的不兼容能力结果都会使该命中失效。命中绝不返回裸 Blob 引用。

## 8. 决策规则

当前 MVP 路由修订（2026-09-09）：只有经过验证的 ACCEPT 才能继续进入 Solution。REJECT、复核区间以及证据不足的已完成业务决策进入 NEEDS_REVIEW，不做变更也不自动重新检查证据。提供方/传输失败仍走既有的类型化 BLOCKED/恢复路径。以下决策值仍是证据分类；它们不是变更派发授权。见 [正向闭环计划](../superpowers/plans/2026-09-09-mvp-generation-loop.md)。

版本化策略将证据映射为：

- ACCEPT：低于接受阈值且具备必需覆盖；
- REJECT：达到或超过硬拒绝阈值且证据充分；
- NEEDS_REVIEW：处于配置的模糊区间内或属于指定来源；
- BLOCKED：无法安全获取必需证据时。

决策存储规则 ID、阈值、精确证据 digest 和题包安全解释码。豁免绑定同一策略和证据，不能自动覆盖后续 revision。

## 9. 隐私与保留

请求省略题解、测试、密钥、本地路径、隐藏提示词和私有来源。日志存储 digest、大小、提供方身份、耗时、状态和净化后的错误。保留策略和题包投影按字段显式定义。

## 10. 测试

测试覆盖规范投影、Unicode 规范化、精确阈值边界、稳定排序、HTTPS 与主机策略、重定向拒绝、响应限制、超时与取消、有界重试、未知发送处理、缓存来源追踪、陈旧健康数据拒绝、人工恢复依赖校验、复核决策、隐私以及题包安全证据。
