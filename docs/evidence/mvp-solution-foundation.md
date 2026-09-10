# MVP Solution 接线进展

日期：2026-09-09。对应 [当前闭环计划](../superpowers/plans/2026-09-09-mvp-generation-loop.md) 的 LOOP-01 / SOL-01。Solution 已完成至公开 CLI 的编译/样例验证边界；完整 Data/Judge/Quality/Package 闭环仍未完成。早期记录保留各自验证范围，最新接线见文末。

## 已实现与验证

- `SolutionDraftV1` 只接受 Reference、Brute 和说明，拒绝模型提交身份、digest、验证结论及非法源码；源码原始字节保留。`SolutionContent` 绑定冻结请求、ProblemSpec 和当前 ACCEPT 证据。
- 编译内置 `solution.draft` prompt/schema，复用现有私有响应、缓存与一次 JSON 格式修复。SQLite + HTTP fixture 验证原始调用及修复调用可恢复，不额外发送、不消耗 mutation 配额。
- `SolutionExecutor` 从已提交 Similarity 证据重新判定输入。ACCEPT 可收集草稿；REJECT、复核区间和已完成但不足以接受的证据返回人工复核，不发 Solution HTTP。新的 Solution revision 现已通过显式配置和 Bootstrap 对外提供。
- M25 增加 `DOCKER_VOLUME_CREATE` 物理调用类型，保留旧 migration 字节与调用记录。卷创建不占用容器创建计数；后续装配用确定性资源计划约束数量。M24 升级测试保留 PREPARED / 已完成调用、预算预留、终态投影和外键关系。
- 正式 `NewPreparedSandboxAuthorization` 只从真实已预留调用构造容器/卷创建能力；每个资源使用独立 logical call 和一个 physical key。SQLite 测试覆盖请求、资源顺序、预算绑定、重复领取、DISPATCHING 拒绝、未发送清理及心跳版本不改变请求身份。
- Docker Runner 保留产物 writer 的调用 ID；目标容器/导出调用保留在执行记录和 CallTrace。产物元数据按值比较，支持 SQLite 重建 provenance；可配置单次操作路径前缀，避免同阶段 Reference/Brute 产物路径冲突。带 `cpgen_slice0_probe` 的 Docker 与 probe 测试通过。
- `SandboxArtifactSink` 按冻结操作声明写入源码、程序、流和结果文件。完成时结算真实物理新增字节；重新读取校验 blob，并重放既有 publication。SQLite 测试验证更换字节/元数据被拒绝、未发布写入释放预算、同一产物阶段附件不重复计费。
- `DockerSandboxSession` 已装配正式资源授权、SQLite 调用/生命周期、产物存储与 prepared detached watchdog，实现 `MeteredSandbox`。每次操作保存绑定完整请求、工具链、Engine 和控制限制的结果收据；只有匹配的 CLEANED 生命周期才能返回成功结果或重放。

## 本地门禁记录

- Solution 草稿及 ACCEPT 分流组件完成后：`go test ./...`、`go vet ./...`、完整 `go test -race -timeout 30m ./...` 通过；竞态测试 application 396.282s、SQLite 502.139s。
- M25、正式资源授权及 transfer 构建修复后：全量普通测试、`go vet`、26 文件架构检查、`git diff --check` 通过。后续 Runner 产物 ID / 前缀修复另有定向 tagged Docker/probe 测试通过；不把更早的全量结果记作后续修改的竞态验收。
- Runner 修复和 `SandboxArtifactSink` 完成后：完整竞态测试通过，SQLite 517.355s、application 365.408s；新增实际 Docker session 随后通过普通和竞态运行，完整普通测试 / vet / 架构检查也通过。最终 11 个产物的原子附件场景在实际 Docker + SQLite + detached watchdog 下通过竞态测试（18.041s）。
- 最终产物收集/附件改动后再次通过完整普通测试（application 38.991s）、`go vet`、Linux/amd64 CLI 编译、全 Go 文件 gofmt 检查、26 文件架构检查及 `git diff --check`。

## 真实 Docker 状态

本机服务 API 1.55 可用。原仓库锁指定的 transfer 镜像缺失；旧 Dockerfile 用 Go 1.24 编译已要求 Go 1.25 的主模块，实际重建失败。`cpgen-image-lock` 已改为用项目已安装工具链和本机模块缓存离线交叉编译可信 transfer helper，再使用仅含 helper 的临时 context 构建锁定 Debian 基础镜像。Docker 构建仍为 `--network=none`；用户程序 builder/toolchain 未改变。

实际命令 `go run ./cmd/cpgen-image-lock --output .tmp/mvp-docker-v1.lock.json` 已成功，开发锁 digest 为 `sha256:4ba96cd4de6d1a06acc14821dd9a681adc6cda86381004f6b7f4a2170a8b0cce`。原仓库锁未覆盖。Docker canary 可通过 `CPGEN_DOCKER_TOOLCHAIN_LOCK` 选择绝对路径。

使用开发锁运行旧 `TestSlice1DockerAB` 已通过静态镜像检查，但旧 memory harness 没有装配当前 Runner 要求的持久化 lifecycle / call ledger，真实编译没有通过。此结果不是 Docker/Judge 验收通过；测试已增加业务状态和失败原因检查，避免只报告缺少物理调用数量。

随后新增正式装配的 `TestDockerSandboxSessionCompilesRunsAndReplaysWithSQLite`，使用相同开发锁实际编译 C++ 整数求和程序，输入 `7 11` 得到 `18`。编译产生 6 个物理资源调用（2 卷 + 4 容器），运行产生 3 个（1 卷 + 2 容器）。重建 session 后两者均从原收据重放，调用身份不变；最终容器预算仍只消耗 6 次。源码、程序、stdin/stdout/stderr、执行记录及结果收据共 11 个产物在同一个 FinishStage 中成功附加。

复现：设置 `CPGEN_RUN_DOCKER_CANARY=1` 和指向开发锁的绝对路径 `CPGEN_DOCKER_TOOLCHAIN_LOCK`，执行 `go test -race ./internal/application -run '^TestDockerSandboxSessionCompilesRunsAndReplaysWithSQLite$' -count=1 -v -timeout 4m`。这是固定程序的真实执行底座验收，尚未把 LLM 产生的 Reference/Brute 接进 Solution 验证，也不是完整 Judge 或题包验收。

## 已提交草稿到真实 Solution 验证

新增 `SolutionVerifier` 和 `SolutionExecutor.VerifyDraft`，新 revision 的固定阶段为 `idea → statement → similarity → similarity_decision → solution → solution_verify → solution_checkpoint`。验证阶段只能读取当前已提交 ACCEPT 和 Solution 私有草稿；普通调用方不能用自造的“已通过”内容启动验证，也不能在阶段已完成后再次调用。旧 preview revision 保持原语义，新 revision 尚未通过公开配置启用。

Reference 和 Brute 的原始源码分别发布、编译，并按顺序执行每一组题面样例。编译采用锁定 C++20/Go 工具链和固定上限；样例继承题面的时间/内存限制。输出按 `exact-tokens-v1` 核对，差异、CE、TLE 等首个内容失败立即停下，不重新生成代码。完成验证的失败报告也以成功的证据阶段提交，后续业务门禁据此进入复核；基础设施错误仍返回错误，不能生成通过报告。样例通过不等于完整 Judge/Quality 通过。

`ReadCommittedSandboxStage` 只读取当前成功 attempt 的已保留本地产物。`SolutionExecutor.ReadVerification` 重建精确编译/运行请求和 Docker scope/plan，逐项核对报告、结果收据、源码、程序、输入/输出、调用身份及 CLEANED 记录，并重新计算实际 stdout 的 token digest；未提交报告、缺失产物、其他草稿或改变执行配置均拒绝。它只读，不新建容器或调用模型。

真实 `TestSolutionExecutorRealDockerVerificationAndReplay` 使用本地 HTTP 生成/查重 fixture、真实 SQLite 和 Docker：BFS 标程及 Floyd-Warshall 小规模 oracle 检查两组图样例。通过路径编译 2 次、执行样例 4 次、创建容器 16 次、提交 33 个产物；首个 WA 路径分别为 2 / 1 / 10 / 19；首个 CE 路径为 1 / 0 / 3 / 6（CE 后不运行二进制提升 helper）。重建 executor/session 后报告与附件的序列化内容完全相同，预算和外部调用数不变。三个路径都验证了阶段提交后的只读重建；未提交、配置改变、输入改变、缺少源码/stdout、额外产物会被拒绝。

实际执行还暴露 Windows 主进程和 detached watchdog 同时删除控制目录的竞态。Windows 对已标记删除但句柄未关闭的目录可能返回 `ACCESS_DENIED`；现在只对访问/共享冲突在每个精确控制路径上最多等待 1 秒，保留持续失败，不递归删除目录。回归测试使用真实 Windows 目录句柄复现删除间隙，并确认无关文件保留。

复现：设置相同的两个 Docker canary 环境变量，执行 `go test -race ./internal/application -run '^TestSolutionExecutorRealDockerVerificationAndReplay$' -count=1 -v`。模型及查重服务仍是本地 fixture；没有真实付费供应商调用，也没有完整数据、Judge 或题包验收。

## 中断恢复与公开 CLI 接线

`DockerSandboxSession` 现在可在结果收据缺失时，从同一 CLEANED 生命周期、完整 execution.json、保留的 stdout/stderr/program 和既有物理调用中重建结果。编译、运行和 CE 三类收据发布故障注入都通过；恢复不新建资源，不改变调用身份和预算。执行证据不完整或外部边界未知时仍拒绝重新执行，这不代表所有崩溃位置都能自动恢复。

确定性本地 `Publish` 可以在确认同一未封口 writer 身份后重写其私有 staging 文件。真实子进程在开始 dispatch 前和部分写入后退出，恢复保留原 writer/call，最终只计费一次。普通流式 `Prepare` 仍不重放未知结果。取消中断的 Solution 校验先清理 Docker，再释放或保守结算原调用，无额外模型或容器执行。

公开 [solution.example.yaml](../../config/solution.example.yaml) 现支持固定本地 Docker 端点、绝对工具链路径及 canonical digest。配置验证不访问 Docker/文件；Bootstrap 校验实际锁与安装镜像并装配真实 reconciler/watchdog。旧 selector 和省略新配置块的历史快照保持兼容。

`TestSolutionPublicCLIResumesCommittedDraftThroughDocker` 使用本地 TLS HTTP fixture 预先提交 Idea/Statement/Similarity/Solution，随后启动独立构建、未修改的 CLI 进程。子进程没有供应商密钥或测试 transport，仍可重建当前草稿，完成实际 Docker 验证并保留 33 项产物；容器创建预算消耗 16 次。再次 CLI Resume 保持版本、调用和产物不变。测试还通过示例配置执行 validate、零预算 generate 及 review resume。该验收不等于真实付费模型或外部查重服务验收。

本轮先通过完整普通测试（application 39.523s / SQLite 30.317s）、vet、Linux/amd64 编译、格式与架构检查。实际 Docker 的六类结果/收据故障、服务分流、中断取消、独立 CLI 及本地产物真实进程恢复的定向竞态测试共 281.031s 通过。随后 Data 接线修改后的最终完整普通测试为 application 41.182s / SQLite 30.415s；Data/Solution CLI/compiled graph 的定向竞态测试 112.721s 通过，Domain/Config 定向竞态、vet、Linux 编译和 gofmt 也通过。

LOOP-01 / SOL-01 完成于经编译和样例验证的 Solution 边界。当前直接推进 [DATA-01](mvp-data-foundation.md) 的程序执行，再接 Judge、Quality 与 Package；checkpoint 不返回 READY。
