# ADR-0005：Docker 执行生命周期与跨停止制品传输

- 状态：Accepted
- 日期：2026-08-30

## 背景

ADR-0004 确定目标程序直接成为独立容器 PID 1，但还不足以保证一次编译/运行在 CLI 崩溃、Docker 日志持续增长或目标容器停止后仍可安全收敛。容器级 tmpfs 会随目标容器删除，Docker 默认日志驱动也可能在宿主持续占用磁盘；若只由 CLI 内 goroutine 执行超时清理，CLI 被强杀后目标程序还可能继续运行。

一次 Compile/Run 还可能创建 import、keeper、target、export 多个容器。它们都是实际 Docker dispatch，必须逐个计入 `max_sandbox_runs` 并留下独立 `AttemptCall`，不能把 helper 隐藏在一个逻辑调用下面。

## 决策

- 执行协议升级为 `docker-direct-v2`。一个逻辑 Sandbox 操作用稳定 `logical_operation_id` 聚合若干物理 `AttemptCall`；每次真实 `ContainerCreate` 使用独立、已预留预算且经 fencing claim 的 sealed grant。`CompileResult/RunResult/DockerProbeResult` 返回公共 `CallTrace`。
- 只读源码、输入和程序通过按 call 隔离的 Engine named volume 传入；可信、digest 固定的 `cpgen-transfer` helper 从 `OpenVerified` 流写入，不读取 Docker volume 的宿主路径。
- 声明输出写入带硬配额的 local-driver tmpfs named volume。可信 keeper 在目标停止后继续持有该 volume；可信 export helper 再只读挂载并以 `openat2/openat + NOFOLLOW` 流式送入 `MeteredArtifactSink`。目标、keeper、export 都是不同 cgroup，helper 不计入题目资源。
- target/helper 使用 `LogConfig=none`，Runner 在 Start 前 Attach stdout/stderr；读到上限加一字节即停止目标并继续 drain/discard。若 Engine 不支持该组合，只能使用经磁盘增长 canary 验证的有界日志 profile。
- Runner 在任何 Docker 调用前持久化 `SandboxExecution` 和完整 PLANNED resource set（确定性 name/labels/call role），并把 `resource_plan_digest`、`engine_identity_digest` 固定到 execution、授权和 watchdog control record。watchdog 先取得不可扩张计划、订阅 Engine events 并完成按资源类型的基线扫描；Docker resource 以 name/labels/Engine identity 收敛，release cgroup 以受限相对路径、creation nonce、owner epoch 和 `populated=0` 证明收敛。每个资源再按“CAS CREATING → watchdog pre-create ACK → Create/mkdir → 持久化 ID/identity evidence → resource ACK → 才允许 Start”推进，因此 Create/mkdir 返回前后崩溃或迟到资源仍可收敛。watchdog 在所有计划 call 可证明终态、最终扫描无运行资源且 execution CLEANED 前不退出。owner 持续监控 watchdog；armed/cleanup 阶段 watchdog EOF/异常退出会立即禁止新 Start 并触发独立 cleanup，startup janitor 可接管。recovery TAKEOVER 只转移 Stop/Wait/Inspect/cleanup custody，不允许继续旧 operation 的 export/Start；未完整持久化的旧 operation 清理后 ABANDONED并以新 logical operation 重跑。watchdog 不产生 Judge verdict 或领域状态，也不自行删除仍有 pin/export 义务的 output volume。
- Orchestrator 取消 context 只触发停止。Stop/Kill/Wait/Inspect/Remove 使用独立有界 cleanup context；目标未确认停止时 run 保持 `RUNNING(mode=QUIESCING)`，并持久化 `CLEANUP_PENDING`，不得提交 `CANCELLED/BLOCKED/READY`。
- `mvp-v2` 以目标容器 OOM event 或 `State.OOMKilled` 证明 MLE，CPU/RSS 可空；capability canary 必须验证 `memory.max`、`memory.swap.max=0`、主进程和子进程 OOM。
- `release-v2` 仅允许批准的 rootful Linux Engine。Runner 在 target 前预建持久父 cgroup，并通过 `CgroupParent` 只放入 target；Wait 后从仍存在的父 cgroup读取层级指标并确认 `populated=0`，再删除父 cgroup。

## 后果

- 编译产物和 generator 输出在 target 停止后仍可验证、提升；不依赖易丢失的容器 tmpfs，也不需要访问 daemon 的宿主 volume 路径。
- CLI 异常退出不再使容器无限运行，但需要维护一个非常窄的同二进制 watchdog 子命令、持久化执行记录和启动 janitor。
- 一次逻辑调用可能消耗多次 sandbox-run 预算；`ContainerPlan` 必须在 dispatch 前确定，MeteredSandbox 必须为每个实际 ContainerCreate 取得独立授权。
- Docker Desktop/local driver、Attach 与日志、keeper 跨停止或 watchdog canary 任一失败时，所选 execute profile 失败关闭为 `BLOCKED`；禁止退回无硬配额 volume 或 host process。

## 参考

- [ADR-0004](./0004-docker-direct-execution.md)：目标程序 PID 1、精确内存 cgroup 和外部 Runner 边界。
- [DockerSandbox 详细设计](../design/sandbox.md)：完整协议、超时、清理和测试契约。
- [Docker volumes](https://docs.docker.com/engine/storage/volumes/)：local driver 的 volume options 由宿主 mount 实现解释；因此 tmpfs/配额/keeper 仍必须逐 Engine canary。
- [Docker attach](https://docs.docker.com/reference/cli/docker/container/attach/) 与 [local logging driver](https://docs.docker.com/engine/logging/drivers/local/)：实时 stdio 与 daemon 日志存储是不同路径；`none+attach` 组合仍以能力测试为准，有界 local 仅作显式兼容档。
- [Docker resource constraints](https://docs.docker.com/engine/containers/resource_constraints/)：`memory-swap` 与 memory 相等时禁止容器 swap。
- [Docker Engine API version history](https://docs.docker.com/reference/api/engine/version-history/)：HostConfig 支持 `CgroupParent`；release profile 仍要求目标宿主上的实际 cgroup v2 探针。
