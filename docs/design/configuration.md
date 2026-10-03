# 配置模型

状态：ADR-0006 下的现行设计

## 1. 来源与优先级

配置是本地配置，按以下顺序分层：

1. 编译期安全默认值；
2. 显式的应用配置文件；
3. 用于密钥和范围明确、有文档记录的覆盖项的环境变量；
4. CLI flag；
5. 策略允许的 GenerationRequest 取值。

合并后，CPGen 会对生效配置进行校验、规范化、脱敏与哈希。不可变的脱敏摘要存储在 run 上。密钥仅按名称引用，永不持久化。

## 2. ApplicationConfig

当前本地 YAML schema 记录在 [config/README.md](../../config/README.md) 中。它接受 `storage`、`sqlite`、`runtime`、`fake_workflow`，以及可选的严格 `llm`、`similarity`、`workflow` 与 `sandbox` 块。[deepseek.example.yaml](../../config/deepseek.example.yaml) 仍是仅含供应商的 Fake 示例；[slice2.example.yaml](../../config/slice2.example.yaml) 显式选择编译进去的 live preview。[solution.example.yaml](../../config/solution.example.yaml) 使已通过的 Similarity 继续完成 Solution 生成与真实 Docker 样例校验，并要求提供本地 Engine 端点、绝对 lock 路径和规范化 lock 摘要。Bootstrap 会检查实际 lock 与已安装镜像；配置校验本身是只读且离线的。仅配置供应商块会保留 Fake 选择。两个 live 选择器都要求冻结的供应商/决策设置以及正的每次交互成本上限。重试与选择规则保持编译期固定。省略新块会保留先前的生效字节与摘要。null、重复/未知字段、非法标量类型与非法限额都会被拒绝。任何环境变量值都不会展开进快照。

`application.BuildLLMConfig` 绑定内置的 prompt/schema 注册表，返回显式输出限额，并将适配器尝试次数固定为一次。可选的格式修复允许量为 0 或 1；其 0 默认值在生效 JSON 中保持省略。显式的工作流选择器会组装持久化 preview 运行时，省略选择则保留 Slice 1 Fake 流水线。live 的 resume/cancellation 需要原始生效摘要。下文更宽泛的配置描述的是目标设计，并非可直接复制使用的本地 CLI 配置。

~~~yaml
schema_version: 1

runtime:
  private_dir: .cpgen
  database_path: .cpgen/cpgen.sqlite
  run_lock_dir: .cpgen/locks/runs
  artifact_lock_path: .cpgen/locks/artifacts.lock
  busy_timeout: 5s
  command_shutdown_timeout: 20s
  active_time_heartbeat_interval: 5s

workflow:
  revision: phase1-v1
  max_stage_attempts: 3
  retry_base_delay: 500ms
  retry_max_delay: 5s

artifacts:
  root: .cpgen/artifacts
  staging_root: .cpgen/staging
  trash_root: .cpgen/trash
  verified_reads: true
  max_single_artifact_bytes: 67108864

llm:
  adapter: fake
  endpoint: ""
  model: ""
  api_key_env: CPGEN_LLM_API_KEY
  timeout: 30s
  max_response_bytes: 1048576

similarity:
  adapter: fake
  endpoint: ""
  api_key_env: CPGEN_SIMILARITY_API_KEY
  timeout: 10s
  max_response_bytes: 1048576

sandbox:
  engine_endpoint: local
  builder_image: cpgen-builder@sha256:...
  runtime_image: cpgen-runtime@sha256:...
  transfer_image: cpgen-transfer@sha256:...
  watchdog_path: cpgen-watchdog
  watchdog_control_dir: .cpgen/watchdog
  stop_grace: 2s
  cleanup_timeout: 20s
  network_enabled: false

logging:
  level: info
  format: text
  redact_private_content: true
~~~

除非有文档记录的管理员策略允许绝对路径，所有运行时与存储路径都解析到显式选定的私有工作区之下。run 锁路径由已校验的 RunID 派生，不能由请求提供。

## 3. GenerationRequest

当前请求使用 `cpgen.request/v1`；完整示例见 [mvp.request.yaml](../../config/mvp.request.yaml)。预算在请求中定义，不属于应用配置：

~~~yaml
schema_version: cpgen.request/v1
mode: manual
brief: Generate an ordinary graph traversal problem with a brute-force oracle.
tags: [graphs]
normalized_tags: [graphs]
language: en
difficulty: hard
required_features: []
forbidden_features: []
time_limit_milliseconds: 2000
memory_limit_megabytes: 512
solution_language: cpp
seed: 9007199254740993
verification_profile: default
export_targets: [internal]
budget_limits:
  max_llm_input_tokens: 150000
  max_llm_output_tokens: 50000
~~~

`max_llm_input_tokens` 和 `max_llm_output_tokens` 分别限制整个 run 累计的输入和输出 token。新请求必须同时填写两项非负整数；额度独立，不能互相借用。两项均为 0 时禁止派发模型调用。重试、格式修复与内容重新生成均计入各自额度，缓存命中不新增模型 token 消耗。调用前分别预留输入和输出上界，返回可靠用量后结算实际值；无法确认用量时保守结算预留上界。模型身份及输入、输出用量仍单独记录，供后续按各自单价计费。

其他调用、时间、容器和存储上限由系统默认策略提供。旧版完整多参数 `budget_limits` 继续兼容，旧 run 保持原有请求摘要、预算及计量语义。兼容的总额字段 `max_llm_tokens` 不能与输入、输出上限混用。

请求字段严格解码。未知字段、重复键、非法 Unicode、非有限数值、不安全路径、不支持的语言、矛盾区间以及策略违规都会在创建 run 之前被拒绝。

## 4. 校验

校验检查：

- schema 版本与工作流 revision 兼容性；
- 私有目录权限；
- 数据库、锁、制品、staging、trash 与看门狗路径不会以不安全方式相互别名；
- 正的时长与有界整数；
- token 预算为非负整数，适配 SQLite 的精确整数表示；
- 镜像引用是不可变摘要；
- 除非显式的本地测试策略另行允许，模型与查重端点使用 HTTPS；
- 端点主机允许列表与重定向策略；
- 密钥环境变量名存在，但不会将其读入快照；
- 活跃时间区间小于相关截止时间；
- Docker 与制品清理超时有界。

不存在针对调度器端点、后台 worker、任务队列或任意阶段图的配置。

LangGraphGo 是由编译期工作流 revision 选定的固定构建依赖，而不是可配置的调度器后端。LangChainGo 使用脱敏后的适配器 Config。本地 `llm` 块提供请求输出上限与响应信封上限；类型化的 GenerateRequest 携带阶段被准入的限额。DeepSeek 示例使用 `https://api.deepseek.com` 与 `deepseek-v4-flash`，凭据仅按环境变量名引用。持久化传输重试由 CallCoordinator 负责，而不是由 YAML 或库的重试设置负责。preview 与 Solution 选择器可通过 CLI 显式使用；完整的 Data/Judge/Quality/Package 选择器在这些阶段实现之前仍是内部选择器。

## 5. 本地锁与计费取值

runtime.run_lock_dir 为每个 RunID 存储一个确定性的操作系统锁文件。runtime.artifact_lock_path 为有状态命令提供共享使用，为 GC 提供独占使用。锁是本地进程原语，没有心跳设置。

runtime.active_time_heartbeat_interval 仅控制计费。该时间戳支持崩溃后的保守计费，且绝不授予执行所有权。

## 6. 供应商与沙箱配置

适配器接收规范化、脱敏后的值对象。阶段代码从不直接读取环境变量。HTTP 策略包括 DNS 与 IP 校验、默认禁用重定向、响应上限、解压上限与超时。

Docker 镜像以摘要固定。沙箱配置选择经过审计的 profile；请求不能削弱挂载、网络、凭据、能力、资源限制、看门狗行为或精确的身份标签。

## 7. 变更与兼容性

配置变更会创建新摘要。resume 校验不可变字段与 run 匹配，并拒绝不兼容的工作流或 schema 变更。显式 revision 会创建新的领域 revision 与下游失效；它绝不静默修改旧证据。

## 8. 命令与测试

cpgen config check 校验配置与可选请求，且不持久化状态。测试覆盖优先级、严格解码、规范化摘要、密钥脱敏、不安全路径别名、非法计时与预算取值、不可变镜像、端点策略以及兼容性检查。
