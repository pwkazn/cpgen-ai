# MVP 最终验收（2026-09-14）

状态：验收完成，**未全项通过**。当前普通 C++ 的确定性 MVP 闭环、完整默认 race 和真实 Docker 故障门禁通过；本轮真实模型因错误样例停止，历史 A/B 探针仍缺生命周期账本接线，真实查重未执行。不能宣布完整发布验收通过。

## 范围与基线

- 基线：`b79023590bb3f3e1d29ba5f0ad0c9a7f1b1b5064`，分支 `codex/application-boundaries`；开始验收时工作区干净。
- 当前 workflow：`mvp.idea.statement.similarity.solution.data.judge.package.v1`。
- 环境：Windows amd64、Go 1.26.5、Docker Engine 29.7.2。
- 工具链：本机已安装的固定镜像，锁文件 `D:/cpgen-private/toolchains/docker-v1.lock.json`；不重建或覆盖原锁。
- 产品范围：普通 C++ 题的生成、审核分流、确定性验证、原子 READY、独立 CLI 导出及导出包复验。
- SPJ、自动变异/内容修复、通用不可信包导入和 Go 实际执行闭环属于后续范围。

真实查重尚无服务/语料配置。本地 Similarity fixture 验证请求、证据绑定和分流机制，不能证明真实检索质量或题目原创性。2026-09-10 的 [APINode 真实模型记录](apinode-live-mvp-2026-09-10.md)属于历史结果，不替代当前提交的真实模型验收。

## 当前门禁结果

| 检查 | 本轮结果 |
|---|---|
| 全量普通测试，无缓存 | PASS；application 59.655s、SQLite 42.865s、integration 7.470s |
| `go vet ./...` | PASS |
| Windows / Linux 命令构建 | PASS；Linux 设置 `CGO_ENABLED=0` |
| `go mod verify` | PASS |
| 独立构建 CLI 校验 `config/mvp.example.yaml` | PASS；返回 `VALID` |
| 全量 race，无缓存 | PASS；application 542.843s、SQLite 653.814s、integration 157.044s，无 race 报告 |
| 历史 probe 标记套件（未启用真实 Docker） | 首轮发现测试夹具时序问题，修正后的 Docker 包 race PASS（6.539s）；同轮 integration/probe/fixture 已通过 |
| 最终格式、架构、文档链接和补丁检查 | PASS；415 个 Go 文件、27 份规范文档、125 个本地链接及 `git diff --check` |
| 修改后的历史测试包 vet（带 probe 构建标记） | PASS |
| 真实 Docker 正向恢复及五类错误内容 | PASS（453.338s）；正向 158.15s、样例错答 12.65s、非法输入 46.59s、不可复现 43.04s、差分错答 93.23s、标程超时 97.83s |
| 公开 CLI 事务中断恢复、离线导出和独立复验 | PASS（188.12s）；移除原工具链文件后仍可恢复并导出 |
| 真实 Docker 取消 | PASS（7.46s）；unsealed_source / compile_receipt 两个窗口均不重新发送 |
| 真实 Docker 编译错误 | PASS（6.443s）；3 次容器创建后停止，未进入 Data |
| 真实 Docker 子进程中断及 watchdog owner EOF | 修正历史测试夹具后，在 race 下 PASS（11.437s）；Crash 6.17s、watchdog 3.83s |
| 历史 `TestSlice1DockerAB` 真实 Docker | FAIL：旧内存 Harness 没有接入当前 Runner 要求的 SandboxLifecycleRecorder / 持久化调用协议；未放宽生产检查 |
| 本轮真实模型 | FAIL（120.256s）：第一组题面样例答案错误，门禁停在 `NEEDS_REVIEW/solution_decision`；详见下节 |
| 真实查重 | 未执行：缺少服务和语料配置 |

普通测试默认跳过显式启用的 Docker 和付费模型场景；这些场景必须以单独结果为准。Go 1.25 与 Linux 真实运行没有在本轮重新执行。

确定性 Docker 正向用例覆盖 Data、Judge、Quality 报告发布及 Package 提交中断；恢复保留原 attempt，不重复计费。通过的题包包含 6 个用例；生成阶段使用 4 次本地模型请求、1 次本地 Similarity 请求、96 次容器创建。源码、上游输入、报告和质量证据替换仍必须被读取门禁拒绝。

五个失败分支分别确认错误样例不能进入 Data、非法/不可重现数据不能成为数据集、差分错误和 Reference 超时不能发布已验证答案。普通 Resume 不增加模型请求、查重请求或变异授权。这些使用固定供应商输出的结果与下面的真实模型失败分别计入结论。

公开 CLI 用例在 Package 事务内部终止真实子进程，确认没有部分提交 READY/VERIFIED；恢复使用原 attempt，导出拒绝覆盖。复验仅使用导出 ZIP，在独立存储和执行身份下重新编译、运行 Reference/Brute。离线导出不增加模型调用、查重调用、run 版本或预算扣减。

## 验收发现与修正

额外启用 `cpgen_slice0_probe` 后，`TestRunnerAcceptsExactTargetOOMEventAsMLEEvidence` 首轮失败；单独连续执行 100 次有 18 次失败。夹具在异步 goroutine 中投递预设 OOM 事件，而内存模拟的目标可能已完成、事件监视器已停止，投递 goroutine 才获得调度。因此原测试并未稳定提供它声称已存在的事件证据。

将预设事件同步放入容量为 1 的消息队列后再返回订阅；仍由 goroutine 等待取消并关闭通道。不改变生产事件监听、停止协议或结果优先级，也不放宽 MLE 断言。修正后连续 100 次通过（1.941s），历史 Docker 完整标记套件在 race 下通过（6.539s）。本轮完整默认 race 不编译这个带构建标记的夹具，因此此修正用单独的标记套件验证。

显式启用历史 integration Docker 用例后还发现以下测试问题，均保留失败日志：

- Crash 恢复子进程已正常输出 `DONE recovery`，父测试却只接受 `READY`。恢复分支现在接受完成信号；后续物理调用 ID、UNKNOWN 结算和 NEEDS_REVIEW 断言保留。
- Watchdog 使用固定容器名造成重复运行冲突，且未将固定镜像自带标签加入精确标签集合。现在每次生成唯一 nonce，并把实际镜像标签纳入预期；没有降低同名外部资源拒绝或精确标签校验。
- Watchdog 夹具在登记 ResourceCreated/TargetPhase 之前启动容器，顺序已与正式 Runner 对齐；异常退出时清理自己创建的精确容器 ID。
- 测试原先要求 watchdog 删除容器，而 [ADR-0005](../adr/0005-docker-execution-lifecycle.md) 的核心职责是目标停止证明，正式实现保留容器供前台核验和删除。现在要求 Inspect 明确报告非 RUNNING 且 PID=0，再由测试清理该精确 ID；不是把运行中的目标视为通过。
- 子进程失败诊断补充 stdout 和 watchdog marker，避免只显示空 stderr。修正后 Crash/Watchdog 在真实 Docker + race 下通过。前几次失败留下的三个测试容器经完整 ID、名称、创建时间和标签核对后清理。

仍未修复的历史 A/B 探针在 `newSlice1DockerHarness` 直接构造 Runner，只提供内存制品和 watchdog，缺少当前要求的生命周期账本。实际返回 `sandbox lifecycle recorder is required for SandboxExecutionID authorization`，随后本地 claim 校验发现未完成调用。补上这一历史 Harness 的账本适配属于后续修复，不能通过删除 SandboxExecutionID 或放宽生产授权检查来让测试变绿。当前 MVP 使用的正式 sandbox Session、CLI 恢复和独立包复验已经通过，不用其结果替代这个失败项。

同时修正计划首页仍称打包未完成、配置说明仍要求新运行保留原工具链文件的问题。TODO 将已交付的模型接线、审核分流和恢复任务与真实查重、查重证据缓存复用分别记录；没有把历史 fixture 结果提升为真实查重验收。

模型凭据由用户指定的本地文件读取，仅注入验收子进程；文件名已加入本机 `.git/info/exclude`，密钥不写入配置快照、日志或报告。模型列表探测不计入生成 run 的调用账本，也不代表生成成功或独立证实服务背后的模型实现。

## 真实模型失败证据

本轮 endpoint 为 `https://api.zhuomatech.cn/v1`，服务列出的模型为 `gpt-5.6-luna`。使用现有 `TestLiveProviderMVPWithFixtureSimilarity`，未替换真实模型传输；Similarity 是明确选择的本地 TLS fixture。运行 ID：`run_bd7bedfda65137dd7cc6e88044976bdb`。

模型生成题目 **Maximum Independent Set in a Grid**，要求选择最多的开放格子，使任意两个所选格子不共边。第一组样例为：

```text
3 4
....
.#..
....
```

题面给定答案为 `7`；Reference 和 Brute 都编译成功，但 Reference 实际输出 `6`，首个样例即触发 `sample.1.SOLUTION.WA`，后续 Data/Judge/Quality/Package 不执行。独立枚举全部 11 个开放格子的子集，最大值也是 `6`；一组最优选择为 `(1,2), (1,4), (2,1), (2,3), (3,2), (3,4)`（坐标从 1 开始）。这确认了样例错误，不能将模型样例当作正确性证明。

此 run 使用 3 次模型调用、1 次本地查重请求、10 次容器创建；记录 5285 输入 token、5053 输出 token。账本保守成本扣减为 300000 micro-USD（0.30 USD），并非供应商账单。最终状态为 `NEEDS_REVIEW`，没有 VERIFIED 包或导出的 ZIP；没有豁免、改写已提交内容或自动变异。

独立 CLI 的 `run resume` 返回退出码 6，仍为 `NEEDS_REVIEW`、版本 142；`run export` 返回退出码 9 和 `package export requires READY`，没有创建目标 ZIP。账本中的该阶段绑定原验证报告，`review_waivable=0`。

私有证据目录：`D:/cpgen-private/zhuoma-live-mvp-20260914-01`。保留 `result.json`、`budget.json`、`events.json`、`failure-analysis.json`、SQLite 与原始响应/编译/样例制品。验证报告摘要为 `sha256:d7a72c26299bfab0ee3a54f8c82783bb6607921c7b700f50d17787b171e7b5df`。这些生成内容不提交到源码仓库。

该结果支持“错误内容被阻止出包”，但不支持“本轮真实模型成功生成可用题包”。未通过重新抽样覆盖这次失败；最终结论必须同时保留确定性工程验收与真实生成失败。

后续经用户授权，以原预算的剩余额度临时重生成一个候选，同样被样例验证拦截：第二组样例声明答案为 4，Reference 输出及独立核验的正确答案为 5。未生成题包，临时源码修改已撤回；详见 [单次重试实验记录](mvp-retry-experiment-2026-09-14.md)。

用户进一步要求跑通链路后，[2026-09-15 补充验收](mvp-live-followup-2026-09-15.md) 记录了独立的成功候选：澄清唯一输出与运行约束后，真实模型、Docker 全流程、READY、CLI 导出、ZIP 独立执行和 12 组校验器补验均通过。中间失败仍保留；这不改变本报告原始结果，也不代表真实查重或历史 A/B 探针已通过。

## 可重复执行

本机日志位于 `.tmp/mvp-final-20260914/`，属于被 Git 忽略的验收输出。日志不是源码测试的前置条件。

```powershell
go test ./... -count=1 -timeout 15m
go vet ./...
go build ./cmd/...
go mod verify
go test -race ./... -count=1 -timeout 30m
go test -race -tags cpgen_slice0_probe ./internal/adapter/sandbox/docker -count=1 -timeout 5m
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
git diff --check
```

Linux 构建在独立进程中设置 `GOOS=linux` 与 `CGO_ENABLED=0` 后执行 `go build ./cmd/...`，避免影响 Windows Docker 验收。Docker 命令在完整 race 结束后串行执行。

```powershell
$env:CPGEN_RUN_DOCKER_CANARY='1'
$env:CPGEN_DOCKER_TOOLCHAIN_LOCK='D:/cpgen-private/toolchains/docker-v1.lock.json'
go test ./internal/application -run '^TestDataRunServiceRequiresRealPassingSolutionAndPreservesDraft$' -count=1 -v -timeout 20m
go test ./internal/application -run '^Test(MVPPublicCLIResumesDataDraftAndExportsVerifiedPackage|SolutionRunServiceCancelsInterruptedVerificationWithoutRedispatch)$' -count=1 -v -timeout 20m
go test ./internal/application -run '^TestSolutionRunServiceRoutesAcceptanceThroughRealVerification$/^compile_error$' -count=1 -v -timeout 5m
go test -race -tags cpgen_slice0_probe ./internal/integration -run '^TestSlice1Docker(Crash|WatchdogFailure)$' -count=1 -v -timeout 5m
# 本轮仍失败的历史 A/B 探针：
go test -tags cpgen_slice0_probe ./internal/integration -run '^TestSlice1DockerAB$' -count=1 -v -timeout 10m
```

真实模型重跑使用 [显式验收入口](../../config/README.md#explicit-live-provider-acceptance)，选择全新的绝对私有输出目录；本轮 endpoint/model 如上，每个 run 最多 8 次模型调用。已有失败目录含 `started` 标记，不能复用。任何后续成功运行都应另记证据，保留本轮失败结论。

所有启动的验收命令均已结束；本轮 watchdog 测试容器已清理，真实失败 run 和原工具链文件保留。修改留在工作区，未创建提交或推送。
