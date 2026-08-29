# LLM、Prompt 与结构化输出设计

## 1. 边界

LLM 只提出 `IdeaCandidate/ProblemSpec/SolutionBundle/TestPlan` 等候选，不决定质量门禁是否通过。Step 负责领域语义校验，Orchestrator 负责状态和制品提交，编译器/Judge/Similarity Policy 等确定性组件负责可执行验证。

MVP 实现一个 provider adapter；普通 Step 只依赖 `MeteredLLM`，PROBING 路径只依赖 Orchestrator 专用的 `MeteredDependencyProber`，二者再调用本文件的低层 `LLMAdapter`。所谓 OpenAI-compatible 仅是 adapter protocol 名称，不允许业务代码依赖供应商字段。

## 2. 请求与响应契约

```go
type GenerateRequest struct {
	Prompt        PromptRef
	Schema        OutputSchemaRef
	Variables     json.RawMessage
	Sampling      SamplingPolicy
	MaxOutput     OutputLimit
}

type LLMAdapter interface {
	Generate(ctx context.Context, auth DispatchAuthorization, req GenerateRequest) (LLMAttemptOutcome, error)
	Probe(ctx context.Context, auth DispatchAuthorization, req LLMProbeRequest) (CapabilityAttemptOutcome, error)
}

type LLMAttemptOutcome struct {
	CallID   AttemptCallID
	Response *LLMAttemptResponse
	Failure  *PhysicalPortFailure
}

type LLMAttemptResponse struct {
	RawJSON       json.RawMessage
	RawBlob       *PendingArtifact
	ProviderMeta  ProviderResponseMetadata
	Usage         Usage
}

type GenerateResponse struct {
	Structured    json.RawMessage
	RawBlob       *PendingArtifact
	ProviderMeta  ProviderResponseMetadata
	Usage         Usage
	Provenance    LLMProvenance
}
```

请求中不得出现任意执行身份、工具、shell、文件路径、网络地址或动态 JSON Schema。Step 只能从编译进程序的 allowlist 选择 `PromptRef`、`OutputSchemaRef` 和 `SamplingPolicy`；用户输入只进入经过编码的 `Variables`。Step-facing MeteredLLM 接收该请求，创建/claim AttemptCall 后才把 sealed DispatchAuthorization 与请求分别传给低层 LLMAdapter。

底层 adapter 每次只执行一个已授权 HTTP attempt，`LLMAttemptOutcome` 的 Response/Failure 恰有一个并始终回显物理 `CallID`；429/5xx、协议错误和供应商拒绝是 typed `PhysicalPortFailure`，普通 `error` 只用于 context/内部错误。Adapter 不知道重试、cache 或 logical operation，也不能构造公共 CallTrace。响应/失败 Blob 只能写入该 authorization 预声明的 `MeteredArtifactSink` writer。Metered proxy 核对 CallID、执行传输/大小/usage 校验，把所有物理尝试组装为 `MeteredOutcome[GenerateResponse]`；成功 provenance 内的 CallTrace 必须与 wrapper 相同，失败也保留决定性 result call，Step 再按固定顺序解析 Schema 和领域规则。

Idea Prompt 只输出 `IdeaCandidate[]` 候选字段，不能输出或覆盖 `IdeaSelection`、feasibility verdict、effective seed、request digest 或 revision；这些由确定性 Idea Step 按 [Idea/Statement 契约](./idea-statement.md) 计算。Statement Prompt 只接收 `StatementInput` 已解析出的 selected candidate 和允许公开的 request 约束字段，输出 ProblemSpec 候选；request snapshot、batch/selection/selected idea/policy 的 digest 链由 Step 包装并在提交时校验，不能信任模型自行回显。

## 3. Prompt 版本

目录约定：

```text
prompts/<step>/<version>/
├── system.tmpl
├── user.tmpl
├── output.schema.json
├── policy.yaml
└── README.md
```

- 模板使用 Go `text/template`，启用 `missingkey=error`；变量先按目标格式编码，禁止把用户内容解释为模板片段。
- `PromptRef` 绑定 step、语义版本和上述文件的组合 digest。运行时文件与编译时登记 digest 不一致则失败关闭。
- system/user 消息边界由 adapter protocol 固定；不得让模型输出覆盖 system prompt、Schema、预算或工具权限。
- `policy.yaml` 声明允许的模型能力、采样范围、最大输出、最多结构修复次数和敏感字段策略，内容 digest 进入 PromptRef。
- `policy.yaml` 还必须按变量字段声明本 prompt 的 data classes；渲染前 Privacy Gate 验证它们是 ApplicationConfig `llm.privacy_policy.allowed_data_classes` 的子集。
- Prompt 的行为变化必须增加版本；只改注释且 digest 变化仍会 cache miss，避免猜测是否有语义影响。

## 4. 严格结构化输出

每个 Schema 必须：

- 有稳定 `$id/schema_version`，对象默认 `additionalProperties: false`。
- 限制字符串、数组、嵌套深度和总代码字节数。
- 对 enum、语言、checker kind、复杂度格式和安全相对路径使用 allowlist。
- 将 Markdown、源码和样例拆成显式字段，不能从自由文本中反向猜结构。

处理顺序：

1. 限制响应字节数，校验 Content-Type/供应商 envelope 和 finish reason。
2. 只接受一个 JSON document；UTF-8、重复 key、非有限数字、尾随内容或未知字段均拒绝。
3. 按登记的 JSON Schema 校验。
4. 反序列化为对应 Go DTO，再执行跨字段/领域校验。
5. 领域对象只有在 Step 成功且 Orchestrator attempt 事务提交后才成为 current output。

不得用宽松 `map[string]any`、Markdown code-fence 抽取或静默默认值让无效响应通过。

## 5. 修复与重试

- 传输层 connect reset、429 和明确可重试 5xx 按 provider policy 重试；每个物理 HTTP attempt 都先取得独立预算 reservation。
- 供应商明确未开始生成时可按预算策略释放 usage 预留；是否已执行未知则标记 `UNKNOWN` 并保守结算。
- JSON/Schema 无效不会作为同一 HTTP 请求的透明重试。Step 可以发起最多一次版本化的 `schema_repair` LLM 调用，输入只含按数据字段编码且受原响应上限约束的无效响应、其 Blob digest、截断后的结构错误和原任务必要上下文；它是新的物理调用、attempt-call ID 和预算记录。
- 确定性修复仅允许移除 UTF-8 BOM 等不改变 JSON 数据模型的规范化，并必须记录修复器版本；禁止猜引号、字段或代码。
- 修复后仍无效，Step 返回 `RetryableFailure(schema_invalid)`；是否重新生成由 Policy 和阶段预算决定。
- 内容违反领域约束时走定向 revision/Agent 重试，不伪装成网络故障。Provider/模型能力暂不可用为 `Blocked`，认证或不兼容配置为 `PermanentFailure`。

## 6. 计量、缓存与幂等

### PricingPolicyRef

`max_cost_usd` 使用版本化本地定价策略确定性核算，不假设 Provider 会返回可信金额：

```text
PricingPolicy
  schema_version, policy_id, currency(USD), accounting_unit(micro_usd)
  rates[]
    effective_model_identity
    usage_category(input|cached_input|output|reasoning|...)
    micro_usd_per_million_units
  input_counter_ref/version, policy_digest
```

- Policy 文件和 digest 进入去密配置快照、reservation 与 `LLMProvenance`。每个 effective model 及 Provider 可能返回的每种 billable usage category 必须精确匹配；无匹配项在物理请求前失败关闭。
- 调用前使用版本化 tokenizer/counter 得到 input units；无法精确计数时使用配置证明安全的 context 上限。output/reasoning 使用请求允许的最大值，cached input 未知时按普通 input 较高费率预留，保证预留是上界。
- 结算使用 Provider 返回且通过一致性校验的 usage units × 固定整数费率；Provider 若返回可信 billed amount 只作对账证据，不替代本地 Policy。usage 缺失/矛盾时按已预留上限结算并 warning。
- 所有乘除法使用溢出检查、整数 micro-USD 和明确向上取整。Policy 变化影响新 reservation/成本报告，但不改变已缓存模型字节；历史 provenance 保留原 Policy digest。
- 该预算是固定 Policy 下的可审计成本上限，不宣称等同供应商最终账单、税费或汇率结果。非 USD 计费必须新增明确 currency policy，MVP 不做动态汇率换算。

### Cache key

LLM cache key 至少包含：

```text
provider protocol + service identity + effective model identity
prompt digest + output Schema digest + normalized variables digest
sampling + max output + adapter version
```

- cache value 保存结构化响应 Blob、原始响应 Blob（若保留）、usage、finish reason 和完整 provenance；命中时仍由 Orchestrator 为当前 run 创建新的 ArtifactOccurrence。
- `effective model identity` 在请求前由配置、别名解析规则和未过期 capability snapshot 得到。响应的 reported model 必须与允许的 identity/alias 集一致，否则响应失败关闭且不写正常 cache；reported value 只进入 provenance，不能在请求后反向改变本次预查键。
- cache 命中不消耗 provider call/token/cost，但必须重新执行当前 Schema 和领域校验；Schema/prompt/adapter 任一 digest 改变即 miss。
- 非零采样结果也可作为“精确重放缓存”复用；不能把同一输入可能产生相同结果称为模型确定性。
- 每个物理请求有稳定 idempotency key。供应商支持时传递该 key；不支持时仅用于本地审计，超时后的远端执行状态仍按 UNKNOWN 结算。
- Provider usage 缺失或自相矛盾时按预留上限结算并产生 warning，不用估算值伪装成实际账单。

## 7. Provenance 与隐私

`LLMProvenance` 至少记录：

```text
provider/protocol/service_identity, requested_model, reported_model
prompt_ref/digest, schema_ref/digest, variables_digest, sampling
call_trace(CallTrace), logical_ordinal, logical_idempotency_key
request_started_at, response_finished_at, usage, finish_reason
raw_request_digest?, raw_response_digest?, adapter_version
pricing_policy_ref/digest, reserved_micro_usd, settled_micro_usd
privacy_policy_version/digest, declared_data_classes[]
```

- 密钥、Authorization header 和供应商请求 ID 中的敏感部分不得进入日志或题包。
- 未发布题面、源码和完整 prompt 默认只保存在本地 BlobStore；普通日志只写 digest、大小、耗时和错误码。
- 发送到模型的每个字段必须被 PromptRef data-class 声明覆盖，并匹配 provider 的版本化 privacy policy。remote endpoint 只有显式 `allow_remote_submission=true` 才可发送；缺失/越权时不创建物理 reservation、不发网络请求，Step 返回 `NeedsReview(llm_remote_submission_not_authorized|data_class_not_allowed)`。
- `api_key/secret`、完整第三方题面/检索正文、宿主路径和未脱敏环境信息是不可授权类别，禁止进入任何 prompt。Similarity 风险反馈只能使用 package-safe 元数据/本地 reason code，不复制第三方正文。
- `doctor` 展示 endpoint identity/class、Policy version 和允许的数据类别，但不发送题目或 prompt。
- 模型返回的 URL、文件名、命令、依赖和“工具调用”都只是字符串数据；除非对应 Step Schema 明确允许并由类型化端口重新校验，否则不得执行。

## 8. Provider capability 与故障

启动/首次调用验证：

- endpoint 可达、TLS/认证配置有效。
- 目标模型可用，支持所需最大上下文/输出和结构化 JSON 模式。
- adapter 能取得或可靠推导 finish reason、usage 和模型标识。

健康快照有短 TTL，不能永久证明模型仍可用。运行中暂时不可达、限流窗口耗尽或模型暂时下线时，在有限物理重试后返回 `Blocked(dependency=llm_provider)`；请求 Schema、认证或 adapter protocol 不兼容则失败关闭。恢复从原 Step 和输入 digest 继续。

## 9. 测试

- Fake provider 覆盖成功、截断、空响应、重复 key、尾随文本、未知字段、Schema/领域错误和错误 finish reason。
- 429/5xx/timeout 的每个物理请求均有 reservation；额度耗尽后不再请求。
- schema repair 只运行一次并独立计费；修复失败返回可审计 RetryableFailure。
- prompt/template/schema digest 任一变化都 cache miss；cache hit 重新校验并创建当前 occurrence。
- provider 报告不同模型、缺失/矛盾 usage、未知执行状态时失败关闭或保守结算。
- effective model/usage category 无定价、Policy digest 变化、整数舍入/溢出和最大 input/output 预留均有固定成本向量；无定价时请求数为零。
- prompt injection、伪造路径/命令/工具调用不能越过 DTO、Schema 和 StepServices 权限。
- public/private remote 未显式授权或 PromptRef data class 超出 allowlist 时，reservation/网络请求数为零并产生可审核证据；不可授权类别始终拒绝。
- 日志、CLI JSON、QualityReport 和题包中不存在密钥或完整敏感请求。
