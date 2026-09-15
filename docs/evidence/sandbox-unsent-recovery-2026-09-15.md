# 首次沙箱调用未发送后的恢复（2026-09-15）

修复范围：`solution_verify` 的第一次 Docker create 在 SandboxExecution 建立前失败，调用已经结算为未发送终止；环境恢复后，公开 `run resume` 所用的恢复服务可以继续验证。Windows ACL 修复保持原样。

基线为 GitHub `main` 的 `7d23677125206e7326e0526551fa46478659884e`（已合并 PR #4，包含 `e5b3393` 的 V2 与 ACL 修复）。本变更直接以 `main` 为 PR 基分支。

## 失败复现

`TestSandboxUnsentVerificationResume` 使用真实 SQLite、生产生成/恢复服务、固定模型与 Similarity HTTP fixture，在 watchdog `Prepare` 中注入错误。普通测试的 Engine 不提供任何 I/O 实现，因此意外访问 Docker 会直接导致测试失败。

先添加测试、后修改生产代码。原实现的首次生成到达 watchdog 故障；第二个服务实例调用 `Resume` 时稳定失败：

```text
resume cannot retry unsent verification:
sandbox requires receipt recovery before another create:
prepared sandbox call differs from exact resource scope
```

原始 [V2 CLI 第一轮证据](v2-cli-live-acceptance-2026-09-15.md) 保留，其已 CANCELLED 的运行没有被重新打开或修改。

## 恢复条件与实现

复用现有阶段中断事务和正常的 `BeginStage`，不增加调用重开、Docker 重发或账本迁移机制。恢复先完成原有沙箱清理与活动时间结算，然后在 SQLite 同一个写事务中检查：

- 当前为 `solution_verify`，且存在 Docker 调用；本改动不扩展其他阶段或纯本地写入失败的恢复策略。
- 当前尝试不存在任何 SandboxExecution，包括已经 CLEANED 的记录。
- 所有调用都是已 TERMINAL 的 sandbox compile/run 调用，只允许 Docker 资源调用和本地 Blob 发布，每个调用恰好有一个物理记录。
- Docker 调用为 `NO_DISPATCH`，物理记录为 `ABORTED_NO_DISPATCH / NO_SEND`，没有 dispatch-start 或 sent 时间。
- 所有预算预留已结束，Docker 调用结算值为零。本地已完成发布允许保留其实际字节费用。

全部满足才将旧尝试记为 INTERRUPTED、当前阶段回到 PENDING。旧调用、物理记录、原始 opening/finish 命令和已消耗预算不变；沿用阶段中断的 writer/pin 释放协议。下一次普通阶段准入使用新的尝试 ID、递增 ordinal、原有已提交 solution 输入和冻结策略，重新申请正常预算并执行验证。上游模型生成、查重和 V2 内容重试额度不重置。

条件不满足时返回原运行，继续既有同尝试 receipt recovery。没有开放新的沙箱权限；准确资源范围、Docker 隔离、watchdog 和 READY 专用事务保持原检查。

Runner 仅在 lifecycle version 大于零时完成持久执行的 cleanup，消除尚未创建记录时附带的 `expected sandbox execution version must be positive` 错误。

## 定向测试

- 普通恢复回归：环境连续失败两次，第二次 resume 到达新的 watchdog 准备调用；旧调用完全相同，输入 digest 相同，尝试 ordinal 递增，Docker 消耗为零，上游 fixture 调用仍为 3 次模型与 1 次 Similarity。
- SQLite 边界矩阵：OPEN、PREPARED、DISPATCHING、SENT、UNKNOWN、COMPLETED、BeginDispatch 后才中止、逻辑调用未结束、其他 provider、未结算的相邻调用、已有 PLANNED/CLEANED 执行和非目标阶段均不获得新尝试。
- 成功中断命令重复执行返回同一结果；模拟中断已提交但尚未 BeginStage 的间隙，普通新尝试使用 ordinal 2；旧调用和整个 BudgetSnapshot 保持相同。
- 过时 run version 被拒绝。
- pending cancel 在新阶段准入前完成 CANCELLED，不增加验证尝试；之后 resume 保持取消状态与版本。

## 真实 Docker 恢复验收

`TestDockerSandboxUnsentVerificationResumeToReady` 使用真实 Docker Engine、固定镜像锁、生产 detached watchdog、SQLite 和完整 V2 生成服务。仅模型与 Similarity 使用本地固定 HTTP fixture，不访问付费模型。

先在连续两次 watchdog 准备中注入相同的执行记录建立前故障，再解除注入并由新服务实例 `Resume`：

- 两次失败期间无 SandboxExecution，预算无 RESERVED，Docker create 消耗为零。
- 第三次 `solution_verify` 尝试成功，沿完整 V2 质量/题包门槛到达 READY；两个旧尝试为 INTERRUPTED。
- 模型 fixture 总计 4 次，Similarity fixture 总计 1 次；未重复发送前三次模型生成或查重。
- 真实容器创建消费 96（上限 256，剩余 160）。
- SandboxExecution 全部 CLEANED；调用全部 TERMINAL；物理调用只有 COMPLETED 或 ABORTED_NO_DISPATCH；预算无 RESERVED。
- 再次 Resume 保持 READY 版本、预算和外部 fixture 调用数不变。

首次执行通过，139.11 秒。增加最终账本审计断言后的复验通过，141.47 秒，运行 ID 为 `run_de93817372ec9fc37331f855f3d68aa7`。额外直接查询 Docker 中带该 run 标签的容器与卷，均为空。

## 验证门禁与环境

环境：Windows/amd64，Go 1.26.5 与 Go 1.25.0，Docker Engine 29.7.2，API 固定 1.55。真实恢复使用本机既有 `D:/cpgen-private/toolchains/docker-v1.lock.json`；没有读取或提交 API 密钥。

本 worktree 的 Go VCS 根目录识别指向 `C:/Users/Ian43`，默认命令构建报 `error obtaining VCS status: exit status 128`。本轮命令以进程级 `GOFLAGS=-buildvcs=false` 禁用构建时 VCS stamp；未更改仓库构建配置或 Git 安全策略。

| 验证 | 结果 |
| --- | --- |
| `go test ./...`（Go 1.26.5） | PASS；application 53.373s，SQLite 34.540s |
| `GOTOOLCHAIN=go1.25.0 go test ./...` | PASS；application 131.462s，SQLite 101.265s |
| `go test -race -timeout 30m ./...` | PASS；application 619.203s，SQLite 784.365s |
| 新增取消边界的定向 race | PASS；14.695s |
| `go vet ./...` | PASS |
| Windows 与 `GOOS=linux CGO_ENABLED=0 go build ./cmd/...` | PASS |
| `go mod verify`、`go mod tidy` 后依赖文件差异 | PASS；go.mod/go.sum 未变化 |
| gofmt、架构脚本、`git diff --check` | PASS；27 项规范文档检查 |
| 更新文档的本地链接 | PASS；70 个 |
| 真实 Docker 恢复与最终账本审计 | PASS；141.47s |

日志保留于本地 `.tmp/sandbox-unsent-recovery/`，不作为仓库内容提交。

仓库默认 lock 的旧 transfer 镜像不在本机，因此依赖该固定路径的历史 Slice 0 Docker canary 未重跑；恢复验收使用完整且可用的私有固定锁。此次是生产恢复服务集成测试，不宣称重新执行独立 CLI 子进程故障验收、真实模型自动重生成或真实 Similarity 接入。
