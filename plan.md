# 算法竞赛自动出题Agent (Competitive Programming Problem Generator) 项目计划

## 1. 项目概述
开发一个多智能体协作系统，能够根据用户需求或随机创意，自动完成算法竞赛题目的全流程生成，包括：创意生成、题面撰写、标准解编写、测试数据构造、非预期解分析与Hack数据生成，最终输出一套完整的、可直接用于OJ评测的题目包。

## 2. 核心目标 (MVP阶段)
优先实现一个端到端的最小可行产品，打通从创意到可评测题目的核心链路。

**2026-09-09 优先级调整：先完成可用出题闭环。查重通过后继续 Solution → 测试数据 → Docker/Judge → Quality → 打包；查重未通过先进入人工复核。变异机制暂停，待闭环可用后重新设计。** 当前执行清单见 [闭环实施计划](docs/superpowers/plans/2026-09-09-mvp-generation-loop.md)。旧变异计划及已完成的研究能力不再作为 MVP 前置条件。
- **输入**：用户的题目关键词、算法类型或简单需求。
- **输出**：一份包含题面(Statement)、标准解(Standard Solution)、测试数据(Test Data)和校验器(Validator)的完整题目包。
- **质量保证**：通过暴力差分、确定性质量门禁和语义查重降低撞题与数据错误风险，并保存可追溯证据；这些检查提高正确性置信度，但不作绝对原创或数学正确性保证。

## 3. 技术选型与资源
- **核心开发语言**: `Go`。负责任务编排、Agent 状态机、CLI/API、缓存、持久化、查重适配器和判题控制；利用 goroutine 和 `context.Context` 实现受控并发、取消与超时。
- **大语言模型(LLM)**: 通过 `LangChainGo` 封装供应商调用，对业务层保留现有 `port.MeteredLLM` 类型化接口。首个集成配置使用已实测的 DeepSeek：`base_url=https://api.deepseek.com`、`model=deepseek-v4-flash`；模型、地址和凭据环境变量名均可配置。
- **Workflow 调度**: 已接入 `smallnest/langgraphgo`，执行编译期组装的 CPGen 固定流程；当前只扩展查重通过后的正向阶段和人工复核分支。保持单机、前台 CLI、每个 run 一个执行者；业务状态、持久化和质量门禁由 CPGen 管理。
- **查重模块 (当前阶段)**: 调用可配置 URL 的语义查重服务，默认 `base_url=https://yuantiji.ac`、`protocol=yuantiji_v2`，通过 `POST /api/search` 查询。公共原题姬与自托管兼容服务使用同一客户端，只需切换服务地址；接口格式不同时再切换协议适配器。
- **查重模块 (未来规划)**: 当项目核心链路跑通后，计划替换为本地部署的 `BGE-M3` 嵌入模型，实现完全离线、自主可控的检索。开发者笔记本（RTX 4060 Laptop 8GB显存）经评估可以流畅运行该模型。
  - **参考项目**: 核心架构与数据格式参考 [fjzzq2002/is-my-problem-new](https://github.com/fjzzq2002/is-my-problem-new) (原题姬)。
  - **关键工具**:
    - 数据生成: 优先使用 Go/C++ 确定性生成器和 `testlib.h`；`CYaRon` 仅作为可选兼容适配器，不作为核心依赖。
    - 向量检索 (后期): 通过统一检索端口接入 Ollama、ONNX Runtime 或独立模型服务；如采用 `FlagEmbedding`/FAISS，则隔离在 Python 服务内，不渗透 Go 核心。

### 3.1 已有基础与集成范围（2026-09-09）

| 基础 | 已验证内容 | 尚待完成 |
| --- | --- | --- |
| `codex/phase2` | 完整 MVP 配置接通正向固定图、真实 Docker、Quality、原子 READY 与 CLI 导出；真实事务内退出恢复和导出包独立编译执行验收通过 | 已验收普通 C++ 题及本地供应商 fixture；外部服务可用性、Go 实际闭环和 SPJ 分别验收。未接受的查重结果仍先复核，变异继续延期 |
| `codex/cpgen-json-demo` / `codex/langgraph-trial` | LangChainGo 供应商适配和图试验；正式分支已锁定 LangChainGo `v0.1.14`、LangGraphGo `v0.8.5` 与最低 Go 1.25.0，并迁入适配器和类型化图兼容回归 | 正式分支的付费供应商、真实 Similarity 和 Docker 端到端验收仍须独立记录，试验结果不能代替正式生命周期门禁 |
| DeepSeek + Docker 实测 | 四个生成阶段成功；保存结果经真实 Docker 编译、12 组对拍及预期输出核对后生成 ZIP；试验支持节点暂停、恢复和有界修复 | 真实查重、完整 Judge/Quality/PackageGate 和正式状态协议；demo 的 `READY` 不作为完整 MVP 验收 |

实现从 `codex/phase2` 的领域契约和已验收执行基础继续，选择性迁移试验适配代码与回归案例。具体清单见 [TODO.md](TODO.md)。现行约束见 [workflow 设计](docs/design/workflow.md)、[LLM 设计](docs/design/llm.md) 和 [ADR-0006](docs/adr/0006-lightweight-local-workflow.md)。当前 [完整包提交证据](docs/evidence/mvp-package-commit-foundation.md) 已覆盖普通 C++ 题从本地供应商 fixture 到真实 Docker、原子 READY、独立 CLI 导出和全新执行复验。详见 [闭环验收范围](docs/superpowers/plans/2026-09-09-mvp-generation-loop.md)；外部供应商与真实查重服务可用性单独验收。

### 3.2 LLM 适配器实现方案

2026-09-09 增量进度：严格供应商配置、单次物理调用、持久化计量、私有响应回放、至多一次 JSON 格式修复以及同 run 私有缓存已接入正式预览并通过完整门禁。`NewStructuredLLMCalls` 保留原调用与修复调用各自的身份和费用；`StructuredLLMCache` 只接纳已提交且重新验证的成功响应，命中后记录当前 attempt 的复用凭据，新增供应商调用与用量均为零。私有产物在阶段提交事务中完成字节结算和生产调用终态。变异 core/intent、独立 mutation prompt、候选收集、结果账本和原子完成也已分别验收；实际变异授权与循环尚未启用。详见 [缓存记录](docs/evidence/slice2-private-llm-cache.md)、[预览记录](docs/evidence/slice2-live-preview.md) 和 [持续开发日志](docs/development-log.md)。

1. **保留端口，封装供应商库**：在 `internal/agent` 增加 LangChainGo 实现，接入 `port.GenerateRequest` / `GenerateResponse` / `MeteredOutcome`。LangChainGo 的消息、模型和响应类型限于适配层。复用现有 HTTP 适配器的端点策略、错误分类和测试契约，先做行为对照，再将真实运行装配切到 LangChainGo。
2. **沿用正式结构化输出契约**：使用已有 prompt 版本、schema digest 和本地严格验证器；保留大小、UTF-8、重复字段、未知字段、整数及领域关系校验。JSON 结构修复默认最多一次，修复输入只带允许披露的错误和片段。不能把 demo 的宽松 DTO 解码直接迁入正式 schema。
3. **把每次物理请求接入持久化计量**：通过 `internal/application/call_coordinator.go` 及现有 ledger 完成预算预留、授权、调用、结算和 `CallTrace`；库内部重试必须关闭或显式纳入同一协议。逻辑操作重试、JSON 修复和业务方案修复各有身份与上限，避免多层重试放大调用。发送结果未知时按既有恢复策略处理，不能因重启自动重新收费调用。
4. **补齐配置和观测**：配置模型、base URL、凭据环境变量名、timeout、最大输出 tokens、响应大小及重试策略；显式处理 endpoint 拼接、JSON 模式、usage、finish reason、限流和截断。复用缓存与当前 run provenance；凭据只从环境读取，不写入配置快照、checkpoint、日志或题包。DeepSeek 数据阶段曾耗时约 117 秒，timeout 与 token 上限应按阶段配置并受总预算约束。
5. **分两层验收**：先以 `httptest` 验证成功、结构修复、429/5xx、鉴权失败、超时/取消、截断、未知发送边界、usage 缺失、脱敏及实际 HTTP 次数与 ledger 一致；再显式启用 DeepSeek smoke。查重 fixture 与真实服务结果分别标明，测试通过后记录正式分支证据。

## 4. 系统架构与工作流 (Workflow)
系统采用**多智能体协作**架构，由总协调器(Orchestrator)调度，核心流程如下：

1.  **创意种子生成**: 协调器启动“随机种子生成器”，通过组合`{数据形态, 算法范式, 故事外壳}`等标签并加入“负向约束”，产生多样化的创意初始点，防止模型创意固化。
2.  **题面生成**: “题面生成器”根据种子，生成结构化题面（标题、描述、输入输出格式、数据范围、样例）。
3.  **查重验证**: “查重模块”通过配置的 `base_url` 调用 Similarity Service；MVP 默认使用原题姬兼容协议 (`POST /api/search`) 进行语义查重。
    - **通过**: 保留当前题面、策略与查重证据，继续 Solution 阶段。
    - **不通过或证据不足**: 先进入 `NEEDS_REVIEW`，由人工处理；不自动变异、不自动重新生成题面。
4.  **标准解与验证器生成**:
    - “标准解生成器”生成 **标准解法(AC)** 和 **暴力解法(Brute Force)**。
    - “验证器生成器”生成 `Validator` (检查输入数据格式)。
5.  **测试数据生成**: “数据生成器”利用标程逻辑和 Go/C++ 确定性生成器批量生成从小数据（用于暴力验证）到大数据（用于压力测试）的测试点；必要时可通过适配器调用 CYaRon。
6.  **判题与质量门禁**: `Judge Harness` 用标程跑所有测试点，调用 validator/checker 验证输入输出，记录耗时/内存并形成质量证据。
7.  **非预期解与Hack数据 (进阶功能)**:
    - “非预期解生成器”尝试生成常见但错误的解法（如错误的贪心、DP）。
    - “Hack数据构造器”分析非预期解的错误模式，针对性地生成能卡掉它的极限数据，迭代增强数据强度。
8.  **Special Judge (SPJ) 处理**: 对于需要SPJ的题目，SPJ生成器与标准解生成器处于**并行且同等的地位**。标准解负责生成正确答案，SPJ负责定义“何种答案可被视为正确”，两者共同构成完整的评测链条。
9.  **完整题目包输出**: 汇总所有产出，按照规范格式（如Polygon格式）打包输出。

### 4.1 完整 workflow schema 的阶段映射

调度库接入沿用现有八个领域阶段。下表列出各阶段需要落实的内部步骤，不能用 demo 的单个 `verification` 节点替代完整验证。字段、证据和失效规则继续以 [workflow](docs/design/workflow.md)、[data pipeline](docs/design/data-pipeline.md)、[Judge](docs/design/judge.md) 和 [package](docs/design/package.md) 设计为准。

| 领域阶段 | 业务步骤与输出 | 交付位置 |
| --- | --- | --- |
| Idea | `GenerationRequestSnapshot → IdeaBatch → feasibility → IdeaSelection`；保留候选、选择理由和变异来源 | Slice 2 |
| Statement | 生成并验证 `ProblemSpec`，绑定已选 Idea、约束、输入输出和样例 | Slice 2 |
| Similarity | `SimilarityEvidence → SimilarityDecision`；有效 ACCEPT 进入 Solution，其余业务结果人工复核；依赖故障沿用 BLOCKED/恢复 | Slice 2 |
| Solution | 生成 Reference / Brute 与说明，Docker 编译并执行题面样例；内容或正确性失败留证并复核/停止 | Slice 3 |
| Data | `TestPlan + Generator + Validator`；编译与 validator 正负例检查、SampleGate，生成可复现的小数据和正式输入 | Slice 4 |
| Judge | 小数据 DifferentialGate；正式输入验证、标程生成答案、checker 判定和 ResourceGate | Slice 3 建基础，Slice 4 完成数据联动 |
| Quality | 汇总同一 revision 的样例、差分、覆盖、资源及查重证据，生成 PrePackageQualityReport | Slice 4 |
| Package | InternalPackage、导出、PackageGate 和 verification receipt；同 run 的已验证 package occurrence 与最终质量报告原子绑定后才可 `READY` | Slice 5 |

### 4.2 Workflow 调度器实现方案

当前固定图已接入正式 Idea/Statement/Similarity 预览及已提交内容重建；提交失败不推进，恢复从当前阶段继续。完整 MVP selector 已接通查重通过后的 Solution、Data、Judge、Quality 和原子打包；旧预览边界不变，见 [闭环验收](docs/superpowers/plans/2026-09-09-mvp-generation-loop.md)。

1. **库与业务边界**：在 `internal/application` 装配 LangGraphGo，将节点桥接到 `internal/workflow` 的具体类型化阶段；领域类型和阶段代码保持不依赖调度库。图在代码中固定组装，第一版串行执行；业务分支限于查重通过后的正向推进、人工复核和停止。自动变异与业务修复不进入首个闭环。
2. **迁移现有执行入口**：从 `LocalRunService` 的 Slice 1 Fake 分派中分离阶段调度，保留 run lock、attempt 开始/提交、预算、取消和 Docker 清理协议。先接入已有 Idea / Statement / Similarity，再随 Slice 3–5 增加其余阶段。未实现阶段明确停在切片边界，禁止用 Fake 结果或空节点补齐正式流程后宣称 `READY`。
3. **单一持久化来源**：SQLite 的 run/stage/attempt 和 Blob occurrence 继续作为正式进度与制品依据；LangGraphGo 持有执行期间的类型化状态。节点成功结果与证据提交完成后才能推进下一节点；从已提交的 stage、workflow revision 和输入 digest 恢复，不并行维护 demo `graph.json`。试验中已发现上游 `v0.8.5` 自动 checkpoint 保存错误被忽略、文件恢复丢失具体状态类型，正式集成必须采用显式检查提交结果的桥接方式。
4. **恢复与控制**：依赖失败进入 `BLOCKED`，人工恢复时重查对应依赖；Docker 故障不能触发内容修复。人工审核沿用 `ReviewDecision`，取消沿用持久化请求和停止证明。首版保留已验收的有界传输重试和 JSON 格式修复，不执行自动解法修复或 Idea 变异。跨进程恢复不得重复调用已提交阶段，外部发送边界未知时沿用保守恢复规则。
5. **版本与兼容**：为新构造器定义 workflow revision，校验 stage 顺序、schema/config digest 与 checkpoint 兼容性；旧 run 继续交给兼容的已编译定义或明确报告不兼容，不能修改旧快照使其强行通过。依赖集成同时升级并锁定 Go 工具链、CI 与构建文档，试验版本只作为首个验证基线。
6. **验收**：用固定普通题和本地 HTTP 场景验证正向顺序、查重未通过后的人工复核、取消及未完成门禁阻止 `READY`；用子进程验证崩溃恢复、同 run 互斥、提交失败不推进及调用不重复。真实 Docker、供应商与查重服务验收分别记录，保留原有 Slice 0/1 安全回归；本地 fixture 不代表真实服务验证。

## 5. 开发里程碑 (分阶段实施)

### 阶段一：核心闭环 (MVP) —— **优先实现**
- [x] 完成运行库集成说明与 Go/CI 依赖兼容调整，明确试验成果迁入正式分支的边界。
- [x] 将 LangChainGo、持久化计量和 LangGraphGo 接入正式 Idea/Statement/Similarity 预览，并完成本地恢复验收。
- [x] 实现固定 seed 的 Idea 与题面内容草稿、严格本地绑定和私有响应恢复。
- [x] 集成原题姬兼容 Similarity 适配器和当前证据读取；真实外部服务验收单独保留。
- [ ] **第一优先级**：有效 ACCEPT 接入 Solution，生成 Reference/Brute 并通过 Docker 编译和样例；未通过先人工复核。
- [ ] **第二优先级**：完成可重现测试计划、generator/validator、数据与答案生成。
- [ ] **第三优先级**：完成 Docker/Judge 差分、正式数据与资源门禁，随后执行 Quality/PackageGate 并导出可复验题包。
- [ ] 验收完整正向链路及复核、阻塞、取消、恢复路径；包级 `READY` 仍以 Slice 5 验收为准。
- [ ] **产出**: 能生成带标程和基础数据的非SPJ题目的命令行工具。

### 阶段二：质量增强
- [ ] 根据实际失败案例重新设计简化的变异机制；旧 MUT/BR 方案暂停，不是阶段一前置条件。
- [ ] 引入“非预期解生成器”和“Hack数据构造器”，实现对抗性数据增强。
- [ ] 增加对 **Special Judge (SPJ)** 题目的支持。
- [ ] **产出**: 题目质量显著提升，能生成需要SPJ的题目。

### 阶段三：系统集成与优化
- [ ] （可选）将查重服务 URL 切换至本地 BGE 嵌入服务，实现完全离线查重。
- [ ] 开发Web界面或API服务，提升易用性。
- [ ] 并行化处理，优化生成速度。

## 6. 关键实现注意事项
- **模块化设计**: 所有Agent模块需设计为可插拔，便于后期替换（尤其是查重模块，初期用API，后期可切至本地模型）。
- **沙箱边界**: Agent 只能调用 Go 编排器提供的类型化编译/运行工具，不获得任意 Shell。所有模型生成的标程、暴力程序、generator、validator 和 SPJ 统一通过 `DockerSandbox` 在禁网、限资源、非 root、一次性的 Linux 容器中执行；Windows 使用 Docker Desktop/WSL2，Linux 使用 Docker Engine，不提供本地进程后端。
- **阻塞与审核**: Docker、查重服务等外部依赖暂时不可用时进入 `BLOCKED`，由用户执行 `run resume` 后在同阶段的新 attempt 中重查依赖并继续；相似性证据模糊、内容冲突或按策略需要人工判断的预算耗尽进入 `NEEDS_REVIEW`。
- **数据缓存**: 向量生成和LLM调用结果必须缓存，避免重复计算带来的高昂时间与金钱成本。
- **错误处理**: 首个闭环在查重未通过或生成内容存疑时先进入人工复核；保留有界传输重试和格式修复，不增加自动业务变异循环。
- **提示词工程**: 为每个Agent编写清晰、专业的系统提示词，明确定义其角色、输入输出格式和约束条件。特别要区分“标准解”和“SPJ”的生成目标。

## 参考项目

https://github.com/7oSkaaa/polygon-problems-generator Codeforces Polygon题目生成workflow
