# LLM、Prompt 与结构化输出设计

状态：ADR-0006 下为当前设计

真实的 Idea/Statement 装配使用严格的带版本内容草稿与本地领域绑定。模型不计算领域哈希，也不选择请求/资源身份。内置的历史 prompt 版本仍可用于收据校验；新的草稿 prompt 及其单次格式修复有各自精确的 schema/模板引用。参见[内容草稿证据](../evidence/slice2-content-drafts.md)。

## 1. 边界

模型调用是通过 MeteredLLM 进行的普通阶段内效果。协调器与阶段代码依赖 provider 中立的接口；provider 字段留在适配器内部。模型输出始终是候选结果，确定性校验可能拒绝它。

模型端口不能访问 SQLite、制品目录、Docker、run 锁或不受限的日志。

2026-09-08 的 ADR-0006 修订选择 LangChainGo `v0.1.14`，限定在 `internal/agent` 内，且 `port.MeteredLLM` 不变。`NewLangChain` 共享现有 HTTP 适配器的 endpoint、规范身份、prompt/schema 注册表、错误分类与严格响应校验。契约测试在应用装配变更之前比较两个适配器。

该库的 HTTP doer 限制为每次调用一个物理请求。CPGen 恢复被准入的 top_p 值（该库版本会丢弃它），显式选择 max_tokens，拒绝其他语义请求改写，并且只把规范的 CPGen 头/体转发给受策略控制的传输层。环境中的 OpenAI 配置不能覆盖已配置的 provider，也不能添加 organization 头。SDK 错误字符串与有损响应 DTO 绝不持久化；原始的有上限字节决定校验与缺失用量核算。显式的非完成 finish reason（包括 length 与 content_filter）即使面对语法合法的 JSON 也予以拒绝。

独立适配器本身不提供持久化计量。LLM-03 派发 checkpoint 增加 `port.PhysicalLLM` 与 `execution.LLMCalls`：规划在没有 I/O 的情况下校验同一请求与 endpoint 策略；一次 `GeneratePhysical` 调用最多执行一次 HTTP 交换，并且即使失败也返回核算。CallCoordinator 预留完整的重试计划、提供物理身份、结算每个响应并返回数据库 CallTrace。逻辑响应用量汇总跨多次 attempt 的已结算预留。在未验证定价的情况下，缺失的用量与成本按其显式预留上限计费；这是保守核算，不是 provider 发票。

只有来自 PREPARED 行的新签发 grant 才授权发送。在没有已封存收据的情况下恢复 DISPATCHING/SENT 会解析为 UNKNOWN，且不发出另一次 HTTP 请求。`NewReplayableLLMCalls` 在 provider 派发之前预留私有响应制品槽位，并通过现有的 Blob writer/pin 协议发布请求绑定收据。恢复会完成 SEALED 发布、验证字节，并在恢复 provider 核算之前重新校验严格 schema。已封存发布失败会保留其预留以供本地对账。执行器必须持有 run 锁与共享制品维护锁。仅账本构造器保留 `ErrLLMReplayUnavailable`；两个构造器都不会用另一次付费请求重建已完成的响应。

私有收据体包含结构化输出、白名单元数据与核算；凭证与 prompt 变量被排除。返回的 `RawBlob` 是用于原子阶段提交的待处理 occurrence 证据，不是题包导出。未使用的槽位释放字节预留；已定稿的收据保持 pinned 直到附加。本地发布获得自己的制品 grant，并与阶段附加和字节结算一起原子地到达终态 producing 调用。被拒绝阶段的清理对已发布的物理字节计费，并释放私有 pin。完整验证状态记录在 [replay 证据](../evidence/slice2-private-llm-replay.md) 与后续缓存 checkpoint 中。有界 JSON 修复与私有缓存来源按下文所述实现。显式 preview、Solution 与完整 MVP 选择器现在通过 CLI 接入真实阶段；省略选择则保留 Fake 行为。参见 [MVP 验收](../evidence/mvp-package-commit-foundation.md)。

在派发尝试之后，收据结算使用一个能在调用方取消后仍存活的五秒有界 context。只有当 run 因匹配的待处理取消恰好推进一次，且同一阶段/attempt 仍为 RUNNING 时，SQLite 才接受过期的收据版本；其他版本冲突仍是错误。规划、授权与重试等待保留调用方的取消 context。重试等待遵循持久化退避与适配器 Retry-After 值中较晚者，并各自上限为一分钟。

## 2. 请求与响应契约

~~~text
LLMRequest
  operation_kind
  prompt_ref
  prompt_version
  template_digest
  model_policy_ref
  canonical_messages
  response_schema_ref
  response_schema_digest
  temperature
  max_output_tokens
  logical_idempotency_key
  privacy_classification

LLMResult
  structured_value
  canonical_response_digest
  provider_request_id?
  usage
  finish_reason
  CallTrace
  safety_metadata
~~~

消息使用类型化角色与规范 UTF-8 编码。工具调用、图像、流式传输或 provider 扩展均被禁用，除非后续版本显式加入。

## 3. Prompt 版本

每个 prompt 都有稳定的引用、语义版本、模板摘要、预期输入类型、输出 schema 与迁移策略。run 记录存储每次调用所使用的 prompt 引用与摘要。

模板由类型化值渲染。不可信的请求文本被界定为数据，不能改变系统策略、工具访问、制品路径或预算规则。

## 4. 严格结构化输出

适配器请求所支持的最窄结构化输出模式，然后独立校验：

- 最大编码字节；
- UTF-8 与 JSON 语法；
- 精确 schema 版本；
- 未知与重复字段；
- 必需字段与 enum 值；
- 字符串规范化与长度；
- 整数范围与有限数值；
- 跨字段领域不变式。

provider 声称 schema 成功不会绕过本地校验。无效输出成为带已净化证据的类型化阶段结果。

## 5. 阶段内修复与重试

`execution.StructuredLLMCalls` 最多允许一次配置的 JSON 格式修复。在原始调用之前，它把完整的修复策略、编译后的 prompt 版本与实现 revision 绑定进 provider 策略摘要，并预检两个 prompt/schema 绑定。使用不同配额或 prompt 的恢复会被拒绝。配置默认为零次修复，且只接受零或一。

修复请求包含原始任务变量与规范本地错误码。它绝不包含被拒绝的模型输出、provider 控制的字段路径或片段。符合条件的错误是 JSON 语法、重复/未知字段、schema 版本与类型化解码失败。领域语义拒绝、HTTP/envelope 失败、截断、超限输出、无效 UTF-8 与缺失的本地 validator 不触发格式修复。

符合条件的校验失败会持久化为有界的 `cpgen.llm-validation/v1` 私有收据，其中仅含代码诊断与核算。`ReadFormatRepair` 在授权修复规划之前，会对照确切的终态物理失败校验该收据。这一次修复有自己的确定性逻辑身份与完整的持久化传输计划；它不能递归修复自身的拒绝。`StructuredLLMResult` 分别返回每次调用的轨迹与制品，并汇总两次调用的已结算用量。最终响应保留 producing 调用的轨迹与用量。成功的阶段必须在其原子 occurrence 提交中同时附加校验证据与修复后的输出。

暂时性传输失败可以在同一前台阶段 attempt 与预算内重试。每次物理调用都会获得一条 CallTrace 记录，而逻辑幂等键保持稳定。重试在成功、阻塞、复核、永久失败、取消或预算耗尽时停止。

如果 provider 发送边界未知，适配器在可能时查询原始 provider 身份。否则它保守地结算预留，并返回类型化的复核、阻塞或失败结果。它绝不会仅仅因为进程重启就以新键发送同一逻辑工作。

## 6. 计量与核算

在物理请求之前，MeteredLLM 预留：

- 一次模型调用；
- 所选 tokenizer 策略下的最大输入与输出 token；
- 最大配置成本；
- 用于捕获题包安全证据的可选制品字节。

在响应或失败之后，它在可信处结算实际用量，并存储定价策略、tokenizer revision、请求摘要、provider 身份、计时、重试分类与 CallTrace。缺失的 provider 用量按带版本的规则保守推导。

阶段代码只能看到类型化结果与只读的剩余预算摘要。

## 7. 缓存

规范缓存键包含适配器协议、provider 与模型身份、prompt 引用与摘要、规范消息、响应 schema 摘要、采样参数、策略摘要与隐私分区。

缓存命中必须：

- 校验策略与可选过期时间；
- 保留源调用；
- 验证被引用的 Blob；
- 创建当前 run 的逻辑调用与制品来源；
- 应用文档化的逻辑核算；
- 通过同样的本地 schema 与领域校验。

私有 prompt 或响应绝不放入共享缓存分区。

`execution.StructuredLLMCache` 实现私有的同 run 响应缓存。它规范化逻辑调用身份，同时保留原始 provider 请求策略与所有语义输入。只有阶段已提交的成功输出才能发布；修复后的成功通过其原始拒绝与确定性修复调用来验证。来源是终态的本地 producing 制品调用，其私有收据保留确切的原始 provider 调用。

`CacheService.ReuseValidated` 在创建当前调用来源之前执行所属适配器的严格校验。命中返回零用量响应，带有 CACHE_HIT 轨迹与 `PendingCacheReuse`，不产生新的 writer token 或 provider 物理调用。同一 run 中较晚的 attempt 由 migration 21 允许；跨 run 来源仍被禁止。缓存键不可变，且在此 bridge 中私有条目没有配置 TTL。缺失/损坏的文件、已失效条目与已变更策略不能静默提供输出。应用工厂仍需要把此 bridge 与生成和阶段提交装配在一起。

## 8. 来源与隐私

原始 prompt 与响应默认私有。日志包含 ID、摘要、大小、计时、用量与已净化错误码。题包安全投影必须显式声明并经独立复核，才能成为题包 occurrence。

密钥、API key、授权头、本地路径与私有源内容会从错误、轨迹与导出报告中移除。

## 9. Provider 能力与失败

在命令启动或首次使用时，适配器可以执行一次普通计量能力检查，涵盖 endpoint 可达性、模型兼容性、响应 schema 支持与策略约束。失败会以依赖身份、策略摘要、证据与 retry-after 时间阻塞当前阶段。

人工恢复为同一阶段启动新的 attempt，并在发送普通工作之前通过 MeteredLLM 重复相关检查。先前的健康数据仅为诊断信息。

## 10. 测试

测试使用确定性 Fake 适配器，覆盖规范请求编码、schema 校验、修复边界、稳定身份、物理 CallTrace 序列、取消、超时、响应大小上限、未知发送边界、保守用量、缓存来源、隐私脱敏，以及阻塞重启后的全新依赖检查。

## 契约短语（canonical contract phrases）

架构检查脚本以这些英文短语作为契约锚点：

- LangChainGo
- internal/agent
- CallCoordinator
