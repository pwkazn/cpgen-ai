# MVP Quality 与包格式接线证据

日期：2026-09-09。对应 [JUDGE-01 / PKG-01](../superpowers/plans/2026-09-09-mvp-generation-loop.md)。内部流程已通过真实 Quality 执行并到达打包入口；新版包格式已实现并通过组件验证。实际 run 产物组装、导出、VERIFIED 包与 READY 的专用事务尚未接通。

## 当前 Quality

- `QualityExecutor.ReadInput` 重建当前已提交且通过的 Solution、Data 和 Judge；校验其来源、完整请求、执行收据、原始输出、答案和 CLEANED 记录。失败的 Judge 在 `quality` 入口进入人工复核。
- `QualityReport` 绑定 run、题面、相似性接受决策、Solution/Data/Judge 报告、工具链和固定策略，记录编译次数、样例运行、复现用例、合法输入、正式运行和差分检查数量。
- 固定 C++ checker 在当前工具链中编译。两个固定 canary 分别要求 AC 和 WA，防止只接受或只拒绝的错误实现通过；随后为每份实际数据运行 checker，小数据使用 Brute 输出与 Reference 答案比较，其余使用当前 Reference 输出。
- 当前 checker 运行限制为 2 秒、256 MiB、64 PIDs、4096 字节 stdout/stderr，正常判定 stdout 必须为空。编译或判定失败形成首个失败报告，不能当作 Quality 通过。
- `quality/report.json` 与 checker 源码、canary 文件及全部真实运行证据原子附着于 quality stage。`ReadReport` 重新派生完整请求并核对报告、来源、策略和所有产物，只读路径不产生执行或写入。
- 成功后流程推进到 `package`，目前在那里以 `package_not_implemented` 进入复核。普通 Resume 不绕过复核。Quality 报告通过不等于已存在 VERIFIED 包或 READY run。

## 新版包格式

`cpgen.package/v2` 复用原有安全路径、清单摘要、原子文件写入和反向读取检查，保留 `v1` 的旧布局及可读性。

- 新题面路径固定为 `statement/statement.md`，题面语言单独记录；模型提供的语言不参与路径拼接。
- Source language 明确为 `cpp` 或 `go`，Reference/Brute/generator/validator 必须使用对应扩展名；checker 固定 `judge/checker.cpp`，声明 `exact-tokens-v1` 并绑定已验证的固定源码。
- `data/tests.json` 保存完整 TestPlan、原始 seed、逐例派生 seed、类型、序号和输入/答案 Blob 身份。样例和生成数据使用独立的固定 ID，全部测试文件必须列入索引，并逐一核对实际字节与清单。
- 包内质量摘要、测试索引、相似性接受决策、工具链和公开 provenance 交叉绑定。包内样例输入必须与 samples 文档一致，答案按固定 token 语义一致。
- 严格 JSON 读取复用领域规则，拒绝重复键、大小写字段别名、无效 UTF-8 和不配对 surrogate；未知字段和尾随 JSON 仍被拒绝。

这些文档里的通过状态和摘要是待核验的声明。结构包的反向读取不会因此赋予 READY 权限；后续应用层必须对照同一 run 的当前 Quality 证据。

## 验证与范围

- Quality 组件测试覆盖通过、CE、始终 AC、始终 WA、实际用例 WA 和 checker TLE，验证失败原因与原子产物附着；缺少用例/canary、替换 checker/答案/上游摘要/run 或差分覆盖数量均被拒绝。组件竞态测试通过（33.546s）。
- 真实 Docker 正向路径使用本地模型和查重 HTTP fixture，执行 6 个实际 checker 检查和 2 个 canary，累计 96 次容器创建、4 次模型调用、1 次查重（137.322s）。
- Data、Judge、Quality 三个报告声明前分别注入中断，新的服务实例恢复同一 attempt，不增加容器或模型计费；缺失已提交 checker 源码或负例 canary 输出时，Quality 只读重建失败。真实 Docker 定向竞态验收通过（493.520s）。这是报告发布间隙故障注入，不是新增的真实进程终止验证。
- 包格式组件验证覆盖 C++/Go 源码路径、题面语言、确定性双次构建、独立反向读取，以及语言、工具链、checker、答案和重新计算外层摘要后的 seed 篡改。测试里的包内容是结构 fixture，不宣称其程序已执行。
- 完整普通测试通过（application 49.375s；SQLite 28.972s；integration 5.365s）。包/领域竞态测试通过（2.316s / 2.270s），旧 tagged probe 与包测试通过（1.126s / 1.291s）；vet、Linux/amd64 CLI 编译和 26 文件架构检查通过。

没有调用付费模型或外部 Similarity。下一步直接实现当前 run 的包产物组装、计量和恢复，随后完成专用 VERIFIED/READY 事务、导出与独立 CLI 复验。变异机制仍暂缓。
