# Task 10 fix round 1 report

## 修复内容

- 将 `stableServiceID` 的连字符前缀规范化为下划线，覆盖 owner-side cancel finish/finalize 的终态幂等键；新增真实 SQLite owner-side 取消回归测试。
- `Resume` 在恢复 RUNNING/CREATED run 时先检查并收敛 durable cancel request，避免 owner 死亡后重新进入 stage 而遗漏终态取消。
- crash matrix 的 `blob_sealed`、`blob_published`、`blob_ready`、`occurrence_commit` 通过真实 SQLite artifact ledger 与 CAS writer 的 stage/seal/publish/finalize/occurrence 操作落点；恢复后校验 FINALIZED token、单一 occurrence、canonical blob、settled reservation 和精确预算余额。
- `sandbox_resource` 通过真实 SQLite sandbox lifecycle/watchdog/resource transitions 落点，恢复路径执行 UNKNOWN、cleanup pending、stop proof、cleaned、finish cleanup，并校验无 unfinished execution。
- 增加 live executor + second-handle cancel + owner force-kill + resume 的进程测试；增加 network/Docker/blob/watchdog/reconciler 五类 durable-prep 后 SQLite writer 边界测试。
- tagged Docker canaries 增加真实子进程 force-kill/reopen probe；watchdog canary 使用 detached watchdog 子进程并验证 owner EOF、reconciler 返回和 control envelope cleanup。未设置 `CPGEN_RUN_DOCKER_CANARY=1` 或 Docker 不可用时由父测试显式 skip。
- 修正 SQLite artifact token 恢复时 NULL 时间列的扫描方式；detached watchdog service 退出时清理控制文件目录。

## 验证

- `go test ./internal/application -run TestRunServiceOwnerCancelFinalizesWithValidDeterministicKeys -count=1`
- `go test ./internal/integration -run 'TestSlice1CrashDurableBoundariesConvergeAfterForceKill|TestSlice1ProcessLiveCancelSurvivesOwnerDeath|TestSlice1LockBoundaryExternalAdaptersDoNotHoldSQLiteWriter' -count=1 -timeout=20m`
- `go test -tags=cpgen_slice0_probe ./internal/integration -run 'TestSlice1Docker(AB|Crash|WatchdogFailure)$' -count=1 -v`（当前环境未设置 canary 环境变量，三项显式 skip）
- 全量 `go test ./...`、`go test -race ./...`、`go vet ./...`、tagged vet/race、`gofmt -l cmd internal`、`git diff --check`

