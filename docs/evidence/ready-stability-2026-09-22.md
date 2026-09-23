# READY 稳定性复验（2026-09-22）

当前结论：用户指定服务和 `gpt-5.6-luna` 的连续三个独立新任务全部达到 READY；每轮导出 Docker 复验、两次无凭据恢复、账本审计及全部答案的独立算法校验均通过，达到本轮事先确定的稳定性验收标准。Similarity 使用本地 TLS 夹具，不能据此宣称真实查重服务通过，也不将三次成功解释为未来所有请求必然成功。

## 指定配置与验收标准

- 服务：`https://api.zhuomatech.cn`；现有适配器使用 `/v1/chat/completions`，模型为 `gpt-5.6-luna`。
- 图片中的凭据不得进入源码、配置或证据文件；真实测试仅通过进程环境变量使用。
- 目标：连续三个独立的新任务达到 READY，每个导出包通过独立 Docker 重编译/执行；READY 后两次普通 CLI 恢复保持完整 run snapshot 和所有预算维度不变。
- Similarity 使用本地 TLS fixture；没有真实查重服务配置。

## 先前真实 V3 失败的只读诊断

保留的原始运行：`D:/cpgen-private/zhuoma-v3-live-20260922-01`，run `run_c291cd77df8e8bf74ca58f7c356cb7af`，最终为 `NEEDS_REVIEW/data`。没有修改其数据库、预算、制品或工作流身份。

从只读 SQLite 索引定位并核对 SHA-256 后发现：

1. 第一次生成的数据程序错误地将 `argv[3]` 与裸 `small` / `boundary` / `stress` 比较，接口实际传入 `--kind=small` 等值。首个 generator 执行返回 2，Data 验证正确拒绝。
2. 工作流正常触发一次 Data 内容重生成。第二次模型请求在配置的 180 秒超时边界停止，调用记录为 `boundary_unknown`，因此保守进入待复核，没有重新发送未知边界请求。
3. 原运行共计入 5 次模型调用、36 次容器创建；仍剩 3 次模型调用额度。失败并非额度耗尽。

诊断副本位于工作区 `.scratch/ready-validation/prior-v3-diagnosis/`。它们是失败模型生成的代码与执行证据，不是修复后成功的产物。

## 本轮修正

- 修复 Judge 策略摘要的历史兼容性：旧报告保留 `every-generated-small-v1`，仅执行样例报告采用新策略。另将旧 Solution schema 独立注册，旧 V1/V2 的请求仍使用原 schema 摘要，已有 V3 的 schema 身份也保持不变。固定哈希回归覆盖四阶段 × 三个工作流及其格式修复提示，不从被测函数重新计算预期摘要。
- 历史题包的实际离线导出又暴露了 Windows checkout 的换行转换：内嵌 checker 从原 2891 字节 LF 变为 2962 字节 CRLF，改变 Quality 策略摘要。已恢复 HEAD 的原始源码字节，为该路径固定 `eol=lf`，并增加独立固定源码摘要回归。未修改 checker 算法、旧 Quality 报告或任何业务数据库。
- 新增可选的 `llm.data_prompt_version: v3`，在原提示基础上给出完整 `argc` / `argv` 和七字符前缀剥离示例，避免将 `--kind=small` 误作裸 `small`。旧提示字节保留，省略字段的旧配置行为不变；新示例配置显式启用。三轮真实模型验收均使用该版本，未再出现此前参数解析错误。
- Windows ACL 回归的准备步骤不再无条件请求 `WRITE_OWNER`：先断言临时目录已属于当前用户，再仅设置 Modify-only DACL。保留验证生产权限函数在这种目录上正常工作的原意。修正前该测试在沙箱内外均失败于准备步骤；修正后相关包及全仓测试通过，生产 ACL 代码未改变。
- 真实模型验收入口增加两次无模型/Similarity 凭据的普通 CLI `run resume`，逐次核对完整 JSON snapshot 与预算快照；只有随后离线导出和独立重编译通过才写 `acceptance.json`。
- 复核发现 Windows 环境变量名不区分大小写，验收脚本的凭据剔除也改为按名称 `EqualFold` 比较，覆盖用户使用小写或混合大小写变量名的情况。当前 runner 明确写入大写名称，已完成的第一轮及运行中的第二轮不受该边界问题影响；第三轮将使用修正后的脚本。
- 该 opt-in 真实模型测试的单次调用超时从 180 秒调整为 300 秒；run 原始活跃时间预算和调用/token/成本上限仍然生效。三轮共 13 次真实模型调用均成功，无未知边界或超时；该结果不意味着以后不可能超时。

## 已完成验证

在当前工作区 `D:/proj/cpgen-ai`、Windows / Go 1.26.5、本机 Docker 和固定工具链锁上执行：

| 检查 | 结果 |
| --- | --- |
| `go test ./... -count=1 -timeout 15m` | 最终修复后 PASS，25 个包通过，2 个包无测试；application 57.502 秒 |
| `go vet ./...` | PASS |
| `scripts/check-slice1-architecture.ps1` | PASS，27 个规范文件 |
| `git diff --check` | PASS |
| `TestDataRunServiceRequiresRealPassingSolutionAndPreservesDraft/pass` | PASS，168.76 秒；此测试为 V1，不能代替 V3 覆盖 |
| `TestSolutionExecutorRealDockerVerificationAndReplay/wrong_answer` | PASS，14.17 秒；错误答案被拒绝 |
| `TestSolutionExecutorRealDockerVerificationAndReplay/run_receipt_gap` | PASS，21.68 秒；恢复重放 |
| `TestExecutedSamplesV3SolutionVerificationUsesExecutionEvidence` | PASS，23.73 秒；空模型样例答案不充当 oracle |
| `TestExecutedSamplesV3DataRunServiceFinalizesDockerSamples` | PASS；恢复 checker 原字节后的最终复跑 140.80 秒；V3 经完整门禁达到 READY，最终样例来自执行 |
| `TestExecutedSamplesV3ResumesAfterFinalStatementPublicationGap` | PASS，141.22 秒；同 Judge attempt 恢复到 READY，不重复模型调用或 Judge 沙箱预算 |
| `TestMVPPublicCLIResumesDataDraftAndExportsVerifiedPackage` | PASS，177.69 秒；V1 公开 CLI 恢复、原子 READY、离线导出和 ZIP 独立 Docker 复验 |
| `TestExecutedSamplesV3RealDockerConnectivityAndBruteBoundaries` | PASS，44.182 秒；三例验证独立编译/执行证据及提交报告原始 JSON 回读 |

新增 V3 三例分别证明：错误草稿答案 `1101000` 不参与 oracle，DSU 与独立分量重标记均执行得到 `1100100`；`n=201` 超出 brute 能力时以 `sample.1.BRUTE.RE` 拒绝；两程序输出 `42` 与 `0` 时以 `sample.1.differential.WA` 拒绝。每例还验证不同源码/程序、独立物理调用、Docker 执行记录及退出码，不以模型声明代替真实证据。

原始日志和机器可读汇总保存在 `.scratch/ready-validation/`。真实模型验收入口的编译已验证；未启用付费开关的 SKIP 不计为真实模型通过。

新 Data 提示版本测试验证旧 effective JSON 逐字节不变、显式选择冻结到配置，并将真实 `draftCall`、`reconcileDraftRequest`、已提交 `GenerationReader.readDraft` 推进到请求计划边界，比较三路完整请求一致，同时检查格式修复版本。另有现有持久化读取/重开及格式修复测试通过。日志为 `data-prompt-selection-tests.log`；最终全仓门禁日志以 `closeout-` 开头。

## 历史 READY 的实际兼容性验证

将 `D:/cpgen-private/zhuoma-cli-v2-20260915-02` 的数据库和 132 个 blob 复制到工作区独立目录，仅修改副本配置中的 `state_root`。使用当前源码构建的新 CLI，清除供应商凭据，未发起模型或 Docker 工作：

- `run show run_1b8cbb63b4a8782abce969ff61cd8e55` 返回 READY，版本仍为 349。
- `run export` 成功，ZIP 为 1,078,352 字节，与历史 ZIP 逐字节一致，SHA-256 为 `fefa9b93c140d1c0c49dac07cc78c1dbba2ffebbe94ed86a996cf5c3a7e111f0`。
- 原数据库、原题包摘要不变；副本的 run/version、349 个事件、400 个逻辑调用、410 个物理调用、45 个 SandboxExecution、227 个 occurrence 及一个 VERIFIED 包均未变化。
- 修复前失败与修复后通过的机器可读证据分别保留在 `.scratch/ready-validation/legacy-v2-copy/before-checker-lf-fix-compatibility-summary.json` 和 `after-checker-lf-fix-compatibility-summary.json`。

## 真实模型连续验收（完成）

新的真实模型调用最初被执行环境的自动审批拒绝，原因是需要明确确认外部目的地、发送数据及付费调用范围。用户随后明确回复“允许”，授权最多三轮、每轮最多八次调用；确认前没有绕过审批再次调用。第一轮已于本机时间 15:31 启动，运行目录为 `.scratch/zhuoma-v3-live-20260922-02`，run `run_c10e28a39f01f81cb4e888474cde7db1`。

第一轮完整测试于 691.22 秒内通过，四次真实模型调用后达到 READY。两次普通 CLI 无凭据恢复保持完整 snapshot 和预算不变；离线导出与已提交题包一致，并已在新 Docker 执行中重新编译验证。独立 HLD 程序仅依题面编写，核对全部 9 组输入/答案，0 跳过，其中 8 组另以 BFS 还原路径交叉检查。题包大小 2,379,971 字节，SHA-256 为 `0b7eeac42392e4d04fa1e57ef23c3255ff9357dd596cf46ceb4c516df67f8a3d`。按该 run 精确标签检查 Docker，容器和卷均为 0。

第一轮证据：运行根的 `acceptance.json`、`resume-1.json`、`resume-2.json`、`problem.zip`；工作区 `.scratch/ready-validation/live-round-01.log`、`round-01-oracle/package-verification.json` 和 `round-01-docker-cleanup.json`。

只读账本审计 `live02-audit.json` 通过：READY v674，12 个阶段均成功；4/8 次已发送模型调用，11,143 输入 tokens、8,026 输出 tokens；无内容重试、未知边界、非终态调用及未结清预留；58 个 SandboxExecution、211 个资源记录全部 CLEANED，157 个 blob 的 SHA-256 与索引一致。

第二轮在新的 `.scratch/zhuoma-v3-live-20260922-03` 目录运行，run `run_e9e3d49b8cc48829c7d6287c81cef338`。完整测试 617.39 秒通过，同样完成两次无凭据恢复、离线导出和 Docker 重编译。独立 Floyd-Warshall 与位集 BFS 对全部 9 组数据双重核验，0 跳过；确认最终题包绑定的题面摘要与提取时一致。题包大小 50,939 字节，SHA-256 为 `5542e7a239a606e865332c8e6424d78bf4d00f0eb23e5f61db52d2f3fbcedc8d`。

第二轮只读审计 `live03-audit.json` 通过：READY v601，12 个阶段均成功；4/8 次模型调用，10,867 输入 tokens、6,758 输出 tokens；无内容重试、未知边界、非终态调用及未结清预留；58 个执行和 211 个资源记录全部 CLEANED，159 个 blob 摘要通过。精确任务标签的 Docker 实况检查同样无残留。

第三轮在新的 `.scratch/zhuoma-v3-live-20260922-04` 目录运行，run `run_5be519041b451e4d0c7068dee884d814`。完整测试 830.15 秒通过，两次无凭据恢复、离线导出和 Docker 重编译均成功。仅依题面编写的子树标记计数、未标记叶子剥离两种独立算法核对全部 9 组数据，其中 5 组另用穷举连通子集确认最小解。最大正式数据含 85,000 个顶点。题包大小 3,216,402 字节，SHA-256 为 `e01250db54532ee9092301187605dda4d894064e869934b8395be856cd8f42b2`。

第三轮只读审计 `live04-audit.json` 通过，READY v823，实际使用 5/8 次模型调用。首次 Data 生成的第六个输入触发 `generated.6.run.1.OLE`，工作流通过既有有界内容重生成再次生成 Data，随后全部门禁通过；保留失败报告和重试记录，没有提升资源限制、改写预算或跳过门禁。

该重试的独立诊断为模型生成内容超限：首版第六组是 132,000 点星形树，按源码的文本格式重建为 1,076,912 字节，超出 1 MiB 限制 28,336 字节；前 1 MiB 与真实 stdout 制品逐字节一致。执行记录完整，非 OOM 或 deadline，stdout 截断标志与 OLE 一致。第二版改为 85,000 点链，两个独立执行均退出 0、输出 997,796 字节且摘要相同，并通过 Validator；策略摘要和限制保持原值。属于已被有界重生成纠正的内容错误，无新增基础设施或工作流代码缺陷。证据见 `round-03-retry-diagnosis.json` 和 `round-03-retry-evidence/`，诊断没有再次执行模型程序。

三轮使用同一模型、同一工作流、同一请求内容和不同显式 seed，各自保留原始预算且不修改业务记录。聚合脚本逐项核对三份请求仅 seed 不同、各 run ID 不同、全部验收/恢复/导出/只读账本/独立答案/资源清理证据一致，结果 `.scratch/ready-validation/stability-acceptance.json` 为 `passed`，无待补证据或失败。

| 轮次 | READY 版本 | 真实模型调用 | 完整验收耗时 | 独立答案校验 | 内容重生成 |
| --- | --- | --- | --- | --- | --- |
| 1 | 674 | 4/8 | 691.22 秒 | 9/9 | 0 |
| 2 | 601 | 4/8 | 617.39 秒 | 9/9 | 0 |
| 3 | 823 | 5/8 | 830.15 秒 | 9/9 | 1，输出超限后恢复 |

累计 13 次成功模型请求、38,523 输入 tokens、26,127 输出 tokens，全部 27 组答案通过独立校验。190 个工作流 SandboxExecution、685 个资源记录全部 CLEANED；三个 run 标签下 Docker 容器和卷均为空。账本无未结清预留、非终态调用或未知边界。账本成本是保守结算值，并非供应商实际账单。对三轮根目录和验收目录的 815 个文件进行本地精确密钥匹配，零匹配；仅报告文件数和结果，不输出凭据。
