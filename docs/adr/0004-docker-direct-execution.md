# ADR-0004：目标程序直接运行于独立 Docker cgroup

- 状态：Accepted
- 日期：2026-08-30

## 背景

若可信 supervisor 与目标程序位于同一容器 cgroup，容器内存上限只能设置为“题目限制 + supervisor headroom”。Docker 的 OOM 限制作用于整个容器，目标程序可能占用 headroom 而不触发 MLE；反之，supervisor 自身内存也可能被算入选手额度。在默认非特权 Docker 容器中安全委派并管理嵌套 cgroup 需要额外宿主权限，且不能作为 Docker Desktop/WSL2 的可移植假设。

## 决策

- 每次运行创建一个只包含目标程序及其后代的容器；目标程序通过固定 OCI entrypoint 直接成为非 root PID 1，不经过 shell，也不启用容器内 supervisor/init。
- Docker Engine 对该目标容器设置 `Memory=题目限制`、`MemorySwap=Memory`、PIDs 和 CPU 限制，不增加可信进程 headroom。
- 可信 Go Runner 位于容器外，负责 AttemptCall/fencing、计时、stdout/stderr、ContainerWait/inspect、cgroup 证据、停止与清理，并生成目标程序无法写入的执行记录。跨停止输出传输、脱离式 watchdog 和多容器逐次计量由 ADR-0005 规定。
- Linux release Runner 必须能从预建且在目标退出后仍存在的父 cgroup v2 读取 `cpu.stat`、`memory.peak` 和 `memory.events`。Docker Desktop/WSL2 的 mvp profile 只以 target OOM event/`State.OOMKilled` 作为 MLE 权威证据，CPU/RSS 可以为空；要求 release 能力时失败关闭为 `BLOCKED`。
- 目标进程的非零退出码始终足以产生角色级错误；只有运行时提供权威信号证据时才填写 signal，不根据 `128+n` 伪造信号。

## 后果

- MLE 的硬限制与题目限制完全一致，不存在目标程序借用 supervisor headroom 的路径。
- 容器内不再需要 `SETUID/SETGID` capability、私有 `/control`、nonce、started/final report 协议或可写 cgroup delegation。
- wall time 采用版本化的 Engine start-to-wait 口径，包含少量固定容器边界开销；正式比赛前仍需 release profile 校准。
- 目标 PID 1 未收割子进程属于被测程序行为并受 PIDs 限制；超时、取消或 PID 1 退出后，Runner 必须确认整个容器/cgroup 已停止。
- 本 ADR 只决定目标资源归属；完整执行生命周期以 [ADR-0005](./0005-docker-execution-lifecycle.md) 的 `docker-direct-v2` 为准。

## 参考

- [Docker resource constraints](https://docs.docker.com/engine/containers/resource_constraints/)：Docker memory/swap 配置由 Engine 写入容器 cgroup。
- [Docker runtime metrics](https://docs.docker.com/engine/containers/runmetrics/)：cgroup v2 容器路径与 CPU/memory 指标获取方式。
- [Linux kernel cgroup v2](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html)：`memory.max`、`memory.peak`、`memory.events` 及 delegation 约束。
