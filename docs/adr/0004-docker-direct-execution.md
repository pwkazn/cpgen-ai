# ADR-0004：在隔离的 Docker cgroup 中直接运行目标程序

Status: Accepted（由 ADR-0006 修订）

Date: 2026-08-30

## 背景

评测测量必须描述不受信任的目标本身，而不是某个 shell、辅助程序或导出器。Docker 保持在目标代码之外，且宿主侧的 Runner 必须保留权威的计时、输出、停止与资源证据。

## 决策

- 容器之外的可信 Go Runner 以显式 argv 且不经 shell，直接创建并启动每个目标。
- 目标就是被计量的 cgroup 进程。传输辅助程序、keeper 和导出器属于独立角色，绝不为目标判决测量贡献数据。
- 每个 Docker 动作都属于一个持久化的 SandboxExecution 与精确计划的资源身份。授权绑定 RunID、AttemptID、SandboxExecutionID、逻辑操作、范围、计划与引擎身份。
- 前台 CLI 在可能授权新的 Docker 工作时持有每 run 进程锁。数据库生命周期版本在该进程内拒绝过期或重复的命令。
- CPU、墙钟时间、内存、pids、stdout、stderr、退出状态、OOM 与停止证据由可信宿主代码收集。
- 目标容器没有 Docker socket，除非特定 profile 允许否则没有网络；它们具有只读根文件系统、显式挂载、被丢弃的能力、no-new-privileges、非 root 凭据以及严格的资源限制。
- 跨停止的输出传输、分离式看门狗行为与精确资源清理遵循 ADR-0005。

## 后果

- 评测证据指向目标本身。
- shell 注入与包装器记账的歧义被消除。
- 平台能力检查可能拒绝无法产生所需证据的宿主。
- 清理授权取决于持久化的执行与资源身份，而不是进程祖先关系。

## 被取代的设计

<!-- Superseded design: begin -->
原先的措辞把 Runner 调用绑定到一种分布式栅栏声明。ADR-0006 用由操作系统支撑的 run 锁与期望生命周期版本取代了那种所有权机制，同时保留精确的授权身份。
<!-- Superseded design: end -->

## 参考

- ADR-0005
- docs/design/sandbox.md
- docs/evidence/slice0-verification.md
