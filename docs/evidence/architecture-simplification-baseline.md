# 架构收敛基线（2026-09-13）

基线提交为 `f9785541470273873a3db7af00a1e0edd9ee1395`；开始时只有计划和 docs/README.md 的未提交文档改动。常规 `go test ./...` 全部通过（缓存结果）；阶段修改另执行无缓存定向测试。宿主 Go 1.26.5 windows/amd64，有 gcc。Docker Desktop Linux engine 不在线，锁文件不存在，真实 Docker 和 live provider opt-in 未启用，因此真实容器与付费供应商不在此次基线通过范围。

## 调用与资源

|入口|原装配资源|实际操作资源|
|---|---|---|
|help/version/config|无执行装配|参数、配置|
|doctor/watchdog|独立 Docker 路径|Docker / watchdog|
|run list/show/events|Bootstrap 全链|SQLite|
|run export|Bootstrap 全链|SQLite、Blob、run shared lock → artifact shared lock、完整证据重建和规范 ZIP 比较|
|generate/resume|Bootstrap → bootstrapSlice2RunService → NewSlice2RunService|SQLite、Blob、run/GC 协议锁、provider、固定阶段调度、Docker（Solution/MVP）|
|cancel/review|Bootstrap 全链|控制写入 / 复核；执行或清理路径需要保持原有安全依赖|
|ArtifactMaintenance API（不是 CLI 命令）|Bootstrap 本地维护对象|GC 排他锁、SQLite 元数据、Blob|

MVP 执行：CLI 解析配置 → 本地 SQLite/Blob/锁 → generation/similarity 配置 → Docker preflight 和 detached cleanup → LocalRunService → 固定调度 → executeStage（准入、BeginStage、计时/取消、typed executor、原子提交）→ 下一阶段。最终 package 将质量证据与已验证包原子绑定为 READY。
恢复：Resume 持 run 锁，检查持久化 revision 和配置 → 恢复 artifact writer / 外部调用身份 → Docker 精确清理 → active time 结算 → 当前 attempt 恢复/复核处理 → 从 SQLite 当前投影重新选择阶段。重复调用只能回放既有身份，不能再付费发送。

## 持久化兼容表

所有 revision 使用 `cpgen.request/v1`，workflow digest 保持 `SumBytes([]byte(revision))`。

|revision|固定阶段序列|
|---|---|
|slice1.fake.v1|prepare, exercise, checkpoint|
|slice2.idea.statement.similarity.v1|idea, statement, similarity|
|slice2.idea.statement.similarity.checkpoint.v1|idea, statement, similarity, slice2_checkpoint|
|slice3.idea.statement.similarity.solution.checkpoint.v1|idea, statement, similarity, similarity_decision, solution, solution_verify, solution_checkpoint|
|mvp.idea.statement.similarity.solution.data.judge.package.v1|idea, statement, similarity, similarity_decision, solution, solution_verify, solution_decision, data, data_verify, judge, quality, package|

兼容比较对象：domain 的 schema 常量、canonical JSON 字段集合和顺序、stage input/output digest、prompt/schema/provider/options/content/policy/toolchain digest、operation/logical call/physical call/reservation/writer/occurrence/receipt 身份。本轮不改这些计算函数或 SQLite migration。重点协议包括 cpgen.llm-response/v1、cpgen.idea-batch-publication/v1、cpgen.data-verification/v1、cpgen.judge-verification/v1 与 domain 的问题/解法/数据/题包 schema。

## 可重复的行为比较

沿用 application 的 GenerationExecutorCommitsRepairedTypedChainAndReplaysWithoutHTTP、CallCoordinatorTerminalSuccessReplayUsesOriginalPhysicalIdentity、LLMReplayProcessCrashBoundaries、StructuredLLMCacheProcessCrashKeepsOneReuseIdentity、IdeaBatchOutputSurvivesRealPublicationProcessExit、slice2_process、package_cli_crash/revalidate 测试及 storage 原子提交测试。它们断言调用次数、已提交摘要、预算预留/结算、暂停/终态与 READY 包绑定；不要只比较最终状态。固定调度的 compiled_graph_test/run_graph_integration_test 保留行为覆盖，纯 LangGraph 库契约探针随 A4 替换。

生产可达：MVP、显式 revision 的 preview/checkpoint、Fake CLI、transfer 的独立包检查。旧 Slice2Pipeline 在本次基线中暂时保留；2026-09-14 复查确认其仅由自身测试调用，后续已删除，历史恢复由共享协调器及固定阶段定义承担。mutation 能力属于另行保留的冻结契约。packageprobe 被导出与独立包校验引用，保留。ADR-0006 和架构检查中的 LangGraph 实现约束在 A4 更新；持久化语义不变。

## 收敛实施与保留机制

A1 已接通 Fake/MVP 的 ControlPollInterval；构造默认 100ms。受控时钟通过真实构造测试验证 750ms、不提前查询、取消和 context 退出；等待上限 5 秒只作测试故障界限。

A5 将计时/取消 poller 与 context/join 所有权交给 stageControl；runRecoveryHandler 单独执行既有身份、清理与 active-time 恢复；runReviewHandler 只应用已持久化 review。三个组件都不关闭外部资源，不拥有整个 LocalRunService 指针。顶层仍持锁、分派阶段并负责终态原子提交。Fake 与当前流程均由同一固定阶段定义提供 create/后继序列，删除 nextStage 的重复映射。

|持久化事实|故障用途|创建/提交者|恢复者|
|---|---|---|---|
|run projection / stage attempt|确定当前版本和当前阶段，区分已开始与已提交|运行服务通过 RuntimeStore 原子事务|Resume / runRecoveryHandler|
|logical operation / call|业务幂等身份及重复请求回放|CallCoordinator / 专用调用适配|CallReconciler、stage recovery|
|physical call / reservation|区分授权、发送、未知发送，先预留预算|调用账本的 Prepare/Begin/Complete 事务|既有物理身份保守结算|
|writer token / receipt|进程在发布前后崩溃可恢复，不重发供应商请求|Artifact ledger / publication session|artifact recovery hooks|
|Blob / occurrence / pin|内容完整性、当前 run 的合法来源、GC 并发保护|Blob store + 阶段提交事务|verified readers / ArtifactMaintenance|
|sandbox execution / resource / cleanup proof|精确清理实际创建的容器与资源|Docker runner / detached watchdog|精确 reconciler，未知身份拒绝清理完成|
|review / package record|防止旧 review 越界，READY 与实际包绑定|checked review / FinalizeVerifiedPackage|复核处理器 / 独立完整证据读取|

A6 可达性审计结论：packageprobe 为当前 CLI 导出与独立格式复验所用，保留；Fake 属于公开默认与兼容测试，保留；旧 Slice2Pipeline 当时因自身契约测试而保留，该判断已在 2026-09-14 workflow 清理中纠正（仅需保留持久化契约，不需保留重复执行器）；mutation prompt/domain/ledger 和 SimilarityRoutePlan 属于已冻结预算/身份及回归边界，本轮不删除也不接入自动变异。仅删除已被本地调度取代的 LangGraph 纯库探针和依赖，无迁移删除，无用户数据清理。

### 已发现的导出前提

旧 run 的 frozen config 仅记录 toolchain_lock_path 和 toolchain_lock_digest，没有完整 lock 内容的持久化快照。Solution/Data/Judge 证明读取必须重建精确 Docker plan，不能由摘要逆推出 lock。故本轮查询不要求 lock 文件，导出可脱离 Docker/凭据，但必须保留与冻结摘要匹配的本地工具链文件；文件缺失或内容不符明确拒绝导出。计划原定“已有 READY 在工具链文件不可读时仍可导出”未完成。未来需单独设计 lock snapshot 及历史补录协议，不能借结构重构绕过证明。

## 本轮验证记录

- 当前 Go 1.26.5：`go test ./... -count=1` 全部通过；`go vet ./...` 通过。
- Go 1.25.0：完整 `go test ./...` 通过，冻结修改后 application/cli/config/agent/similarity/workflow 六包再次通过；`GOOS=linux CGO_ENABLED=0 go build ./cmd/...` 通过。
- `git diff --check` 与架构一致性脚本（26 份规范文档）通过。
- 新增 CLI 测试使用真实 MVP 配置、不可用 endpoint/lock 和空凭据验证 list/show/events；冻结配置测试验证无损恢复及错误配置、错 run/attempt、未清理执行、缺 receipt/lock 拒绝。
- 初次常规测试跳过真实 Docker；用户启动 Docker 后已完成下列真实容器补验，不能将初次 SKIP 作为通过证据。
- `go test -race -timeout 30m ./...` 全量通过：application 594.175s、CLI 51.683s、integration 174.590s；其余通过（部分无改动包复用缓存），无 race 报告。

### Docker 补验（2026-09-13）

环境为 Windows amd64、Go 1.26.5、Docker Engine 29.7.2。使用本地已固定摘要的基础镜像，经 `go run ./cmd/cpgen-image-lock --output D:/cpgen-private/toolchains/docker-v1.lock.json` 构建 builder/runtime/transfer；本机锁摘要为 `sha256:dd84cfa46440d3adde9ee4ada4ed9f61b100bca026b191d401f464f3a58d1384`。未替换仓库示例锁。

以下命令均设置 `CPGEN_RUN_DOCKER_CANARY=1`、`CPGEN_DOCKER_TOOLCHAIN_LOCK=D:/cpgen-private/toolchains/docker-v1.lock.json`，顺序无缓存执行，均 PASS，无跳过：

```powershell
go test ./internal/application -run '^TestMVPPublicCLIResumesDataDraftAndExportsVerifiedPackage$' -count=1 -timeout 20m -v
go test ./internal/application -run '^TestDataRunServiceRequiresRealPassingSolutionAndPreservesDraft$' -count=1 -timeout 20m -v
go test ./internal/application -run '^TestSolutionRunServiceCancelsInterruptedVerificationWithoutRedispatch$' -count=1 -timeout 20m -v
```

- MVP CLI（181.75s）：实际容器生成 READY、包事务内真实进程退出恢复；将当前 CLI endpoint/lock 路径改为不可用并清空凭据后导出成功，拒绝覆盖，供应商调用数、预算与 run version 不变；从 ZIP 独立重新编译执行通过。原 frozen lock 文件保持存在。
- Data/完整流程（464.58s）：正常路径在 Data/Judge/Quality/Package 中断后保留原 attempt，不重复扣发布或执行预算；wrong_answer、invalid_generated、nondeterministic、differential_wa、reference_tle 五种负向均按预期阻断，验证证明替换被拒绝。
- 中断取消（10.27s）：unsealed_source 真实进程崩溃和 compile_receipt 中断后取消、清理及账本终结通过，没有重新发送请求。

供应商均为本地 HTTP fixtures；本次没有调用付费模型或验证真实外部供应商。本次补验未改生产代码，原 frozen lock 缺失时无法导出的限制仍保留。
