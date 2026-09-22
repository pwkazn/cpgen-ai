# 本地提供方与工作流配置

`solution.example.yaml` 选择 `slice3.idea.statement.similarity.solution.checkpoint.v1`：
ACCEPT 继续进入 Reference/Brute 生成和真实的 Docker 样例检查；
其他业务决策进入评审。通过的题解停在
`solution_checkpoint`，退出码为 6。该选择器不能产生 READY。历史预览
选择器和 Fake 选择器保持其现有行为。

`mvp.example.yaml` 选择完整的普通题目工作流。查重 ACCEPT
继续经过题解、可复现数据、评测、质量和题包。READY
要求当前已验证的题包事务。未被接受的业务结果
进入评审；自动变更仍然推迟。独立的 CLI 崩溃恢复、导出和执行重新验证通过；见[题包证据](../docs/evidence/mvp-package-commit-foundation.md)。

在配置好端点、凭据、本地路径和钉定的工具链之后，
使用 `mvp.request.yaml` 进行一次显式预算的 run：

```powershell
go run ./cmd/cpgen --config config/mvp.example.yaml generate --request config/mvp.request.yaml
go run ./cmd/cpgen --config config/mvp.example.yaml run show RUN_ID
go run ./cmd/cpgen --config config/mvp.example.yaml run export RUN_ID --output D:/output/problem.zip
```

将 `RUN_ID` 替换为返回的标识符。示例命令使用
Windows 路径形式（`D:/...`，以及 Docker 端点的 `npipe://`）；在 Linux
和 macOS 上使用对应的原生形式（`/home/you/...`、`unix:///var/run/docker.sock`）。
导出要求 READY，会重新检查
当前已提交的证明，并拒绝替换已存在的目标。
目标目录必须存在并支持硬链接。正的示例
预算仅在你调用生成时才允许外部调用；没有任何示例
包含凭据。为 resume/export 保留确切的有效配置。

对于任一前向选择器，配置必需的 `sandbox` 映射：

| 字段 | 必需值 |
| --- | --- |
| `engine_endpoint` | Windows 上显式的本地 `npipe:////./pipe/docker_engine`，或 Linux 上的 `unix:///var/run/docker.sock`；远程 TCP 端点被拒绝 |
| `toolchain_lock_path` | 镜像/工具链锁的绝对路径 |
| `toolchain_lock_digest` | 由 `cpgen-image-lock` 打印的规范 SHA-256 摘要；替换示例中全零的占位符 |

`config validate` 检查封闭的 YAML，不读取锁也不联系
Docker。Bootstrap 在生成之前检查实际的锁摘要、本地 Engine 能力和
已安装的钉定镜像。运行中的 CLI 会启动自己的
分离式看门狗。使用现有的离线工具链/镜像前置条件，
通过 `go run ./cmd/cpgen-image-lock --output
<absolute-lock-path>` 构建镜像。
所有 sandbox 字段都参与有效配置摘要。恢复一次 run 时，
保持原始文件/路径/设置可用。

现有的零预算请求也可以演练该选择器；它在不派发
提供方调用的情况下停在创意，但 Bootstrap 仍要求已配置的
本地 Docker 安装。正预算请求必须覆盖分别
预留的传输尝试、制品和容器创建。一个两样例
通过的题解目前使用 16 次容器创建。示例是说明性的
限额，不是提供方价格或普遍的工作量估计。

`deepseek.example.yaml` 是一个无凭据的集成目标。使用前将其
`storage.state_root` 替换为私有的绝对目录。
端点/模型对采用已实现的提供方配置契约；该示例不做任何实时可用性
声明。这个仅含提供方的示例运行 Fake 工作流。仅提供方配置
本身并不会选择实时执行。

`slice2.example.yaml` 显式选择编译好的实时预览工作流：
创意 → 题面 → 查重 → `slice2_checkpoint`。它要求两个提供方
块和正的每次交换成本上限。最终检查点保留
已评估的查重决策，并暂停以进行不可豁免的评审。该
修订不应用后续的业务路由/变更策略，也不产生
READY；题解、数据、评测、质量和题包门禁仍然必需。

在将 `storage.state_root` 替换为私有绝对路径之后，随附的
`slice2-zero-budget.request.yaml` 可在不派发提供方调用的情况下演练准入：

```powershell
go run ./cmd/cpgen --config config/slice2.example.yaml config validate
go run ./cmd/cpgen --config config/slice2.example.yaml config effective --redact
go run ./cmd/cpgen --config config/slice2.example.yaml generate --request config/slice2-zero-budget.request.yaml
```

最后一条命令会有意在创意处返回 `NEEDS_REVIEW`（CLI 退出码
6；`go run` 本身报告子进程非零退出）。要进行实际生成，请设置
提供方端点/模型/服务身份、凭据和请求预算。
示例上限是说明性的预留限额，不是报价。
传输策略允许每次逻辑调用有两次分别预留的物理尝试；
格式修复在启用时有自己的调用和预留。

所有有效设置在创建时冻结。通过实时服务进行的
恢复和取消会在变更之前拒绝已更改的配置。仅更改
环境变量的凭据值不会改变摘要。为现有 run
保留原始配置可用。

整个 `llm` 映射是可选的。缺省时，保留先前的有效 JSON
字节和摘要。存在时，`base_url`、`model` 和
`api_key_env` 为必需。`api_key_env` 是一个环境变量**名称**；
配置加载、校验、有效输出、哈希和
应用映射都不会查找其值，也不会检查它是否已设置。凭据
的可用性和有效性在提供方派发时检查。

| 字段 | 省略时的默认值 | 可接受的值 |
| --- | --- | --- |
| `base_url` | 必需 | HTTPS 基础 URL，至多 2048 字节；DNS 主机或非本地单播 IP 字面量，可选的合法端口/路径；不含凭据、query、fragment、百分号转义或点路径段 |
| `model` | 必需 | 非空 UTF-8，至多 256 字节；无首尾空白、控制字符或环境变量插值 |
| `api_key_env` | 必需 | `[A-Za-z_][A-Za-z0-9_]*`，至多 256 字节 |
| `timeout` | `30s` | 正的 Go duration，至多 `10m` |
| `max_output_tokens` | `4096` | 1 到 1048576 的整数；应用策略界限，而非提供方能力声明 |
| `max_response_bytes` | `1048576` | 1 到 67108864 的整数 |
| `max_format_repairs` | `0` | 整数 0 或 1；一次可选的 JSON 格式重新生成，单独计量 |
| `data_prompt_version` | 空值，沿用历史选择 | 空字符串或 `v3`；显式 `v3` 仅允许工作流 V3，选择包含完整 generator 参数解析示例的内置 Data 提示 |

未知和重复的键、别名/合并键、null、错误的标量类型、
空的必需字符串以及无效或越界的值都会被拒绝，并给出限定
字段的错误。默认值仅适用于被省略的限额字段。没有
可配置的 prompt 文本、schema、原始 API key、HTTP 测试旁路或适配器重试
字段。超时在有效输出中被规范化为 duration 字符串；所有
提供方字段（包括凭据引用）都参与摘要。
被禁用的修复默认值会从有效 JSON 中省略，以保留较旧的
提供方快照；启用它会改变有效策略摘要。

`mvp.example.yaml` 显式选择 `llm.data_prompt_version: v3`。此版本明确
generator 收到的是 `--seed=42`、`--case=1`、`--kind=small` 三个完整参数，
应分别剥离七字符前缀，并用无符号 64 位整数解析 seed。
省略或设置为空字符串时，有效 JSON 不增加此字段：旧 V1/V2 工作流仍使用
Data 提示 v1，旧 V3 工作流仍使用 Data 提示 v2。显式选择 v3 会改变配置摘要，
仅用于新 run；恢复、读取和格式修复都遵循 run 的冻结选择，不升级历史提示。

`application.BuildLLMConfig(config.Config)` 返回 `(agent.Config,
port.OutputLimit, error)`。它拒绝缺失的提供方，将 `MaxAttempts` 固定为
**1**，并安装全新的内置创意/题面 prompt 和 schema 注册表。
schema 摘要标识编译好的领域解码/校验契约；prompt
是带版本的定义，领域和输入链校验仍由
有类型的阶段执行器完成。该辅助函数不执行任何凭据或
网络 I/O，也不构造工作流。编译好的格式修复变体
使用相同的输出 schema。`BuildFormatRepairPolicy(config.Config, step)`
选择创意或题面修复变体以及配置的允许次数。

`NewStructuredLLMCalls` 在第一次付费调用之前绑定该策略，在本地
校验两个 prompt 绑定，并且仅对列入允许清单的 JSON
格式错误允许一次修复。它传递原始任务输入加上经净化的错误码；
被拒绝的模型文本绝不会复制到修复 prompt 中。传输和领域
失败不会补充或消耗该格式允许次数。每次逻辑调用
保留自己的 trace、私有制品和持久用量核算。

返回的输出限制必须在持久账本准入之前应用到
`GenerateRequest.MaxOutput`；阶段可以选择更小的限制。其字节上限
也会限制整个提供方 HTTP 响应，因此信封开销也计入
该上限。`agent.Config` 本身没有 token 限制字段。传输重试
属于持久调用账本。DNS/IP 校验、重定向、解压
限制和超时仍由现有适配器在派发时强制执行；配置
校验不解析主机名。

可选的 `workflow` 字段有 `revision`（确切的预览、题解或 MVP 修订）、
`idea_count`（2–8，默认 4）、`llm_cost_upper_bound_micro_usd` 和
`similarity_cost_upper_bound_micro_usd`（必需的正整数，各自至多
`MaxInt64/8`）。YAML 中不接受任意的阶段顺序、prompt、重试程序或模型
指令。

可选的 `similarity` 块要求 `endpoint`、`api_key_env`、
`provider_identity`、`service_identity`、`policy_ref`、`acceptance_threshold`
和 `rejection_threshold`。端点和凭据引用校验遵循
LLM 规则。阈值必须是 [0,1] 内的有限值，且 acceptance ≤ rejection；
整个间隙就是评审区间。`timeout` 默认为 `30s`（最大 `10m`），
`max_response_bytes` 默认为 1048576（最大 67108864），`limit` 默认为 20（1–10000），
`minimum_hits` 默认为 1（1–limit）。索引或服务修订参与
冻结的提供方策略。省略 `workflow` 和 `similarity` 会保留
先前的有效字节；提供方块可以在没有实时选择的情况下存在。

物理调用重放和终态提供方对账已与
预览 run 服务集成。其验收使用本地 HTTP fixture；未声称
任何付费提供方或 Docker 冒烟结果。

## 显式的实时提供方验收

`TestLiveProviderMVPWithFixtureSimilarity` 使用真实配置的模型和
Docker；只有查重被替换为本地 TLS fixture。它要求以下所有
进程环境变量：

- `CPGEN_RUN_LIVE_MVP=1` 和 `CPGEN_RUN_DOCKER_CANARY=1`。
- `CPGEN_LIVE_BASE_URL`、`CPGEN_LIVE_MODEL` 和 `CPGEN_LIVE_API_KEY`。
- 一个绝对的 `CPGEN_DOCKER_TOOLCHAIN_LOCK`，指向已安装的钉定镜像。
- 一个全新的绝对私有 `CPGEN_LIVE_ROOT`，用于保留的证据和导出。

运行 `go test ./internal/application -run '^TestLiveProviderMVPWithFixtureSimilarity$' -count=1 -v -timeout 30m`。
这显式授权该测试最多八次计量模型调用，
包括有界的格式修复。该测试没有隐式的端点/模型，并
拒绝复用已启动的 root。它在派发模型之前构建自己的 CLI，
记录阶段进度和预算，并要求 READY 加上独立的 ZIP
编译/执行通过。默认测试运行会跳过所有实时请求。

可选地将 `CPGEN_LIVE_REQUEST` 设置为包含完整
`cpgen.request/v1` JSON 文档的绝对路径。验收测试严格解码它，不
合并默认字段，并将所选请求作为 `request.json` 保留在
全新的私有 root 中。其显式预算限额适用于该 run。

配置实际的 API 前缀：对于所测试的 APINode 服务使用
`https://apinode.ltd/v1`；其根路径 `/chat/completions` 返回了 HTML。
凭据只应放在进程环境中，不要放在 YAML 或日志里。
提供方 fixture 测试和成功的认证并不能确立一次
成功的实时生成 run；请查阅保留的验收结果。

MVP 示例现在选择工作流 `mvp.idea.statement.similarity.solution.data.judge.package.v3`。
工作流版本保持向后兼容：V1 是原始的固定流程，V2 增加每个 run
最多两次有界内容重新生成，V3 在此基础上加入独立执行样例定稿。
V2/V3 每次 run 最多允许两次自动内容重新生成，使用现有的
调用/token/成本/时间预算。它在绑定/JSON 格式
失败后重新生成草稿，在编译/评测失败后重新生成题解，在样例失败后重新生成
题面及其下游阶段，在 generator/validator 失败后重新生成数据。
每次验证都会重新运行；失败的尝试仍可在 SQLite 的
`content_retries` 和普通调用/阶段记录中审计。耗尽后返回 NEEDS_REVIEW。
这是从原始输入重新生成，而非诊断引导的修复。
冻结的 V1/V2 run 保持其原有行为；创建新 run 时选择 V3。


## 执行样例工作流

`mvp.example.yaml` 选择 workflow v3 和 `cpgen.package/v3` manifest；V1 保留历史固定流程，V2 增加有界内容重生成，V3 在相同预算边界内加入独立执行样例定稿。V1/V2 旧 run 由兼容路径按持久化 workflow identity 读取，新 run 使用 V3 配置；详见[样例策略与恢复](../docs/design/executed-samples.md)。
