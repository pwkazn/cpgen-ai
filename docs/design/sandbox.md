# DockerSandbox 详细设计

状态：ADR-0006 下现行有效

## 1. 目标与边界

DockerSandbox 执行不受信任的编译、目标程序、checker 与传输辅助程序，同时保留权威的主机侧证据。它保留在 Slice 0 中已验证的 docker-direct-v2 Runner 与分离式看门狗。

目标代码绝不会获得 Docker socket、制品存储凭据、本地数据库、run 锁、看门狗控制或显式挂载之外的主机路径。


### CPU 时间 TLE 证据

正式的资源门禁必须使用所选 profile 的权威 CPU 时间证据，判定目标是否超出 CPUTimeLimit。Docker CPU 配额限制的是 CPU 可用量；它并不测量累计 CPU 时间。为停止安全起见，墙钟截止时间仍然是强制的，但无法证明 CPU 时间 TLE。

在 mvp-v2 过渡期间，编译、样例以及参考解对暴力解的功能检查可以继续。如果无法权威地测量 CPU 时间，cpu_time_ms 必须保持为 null，且不能支撑资源门禁或题目时限。资源门禁返回 BLOCKED，并带 capability_missing=cpu_time_measurement。只有通过 CPU 时间能力金丝雀的 release profile 才能产出最终的 CPU 时间 TLE 证据。

## 2. 能力检查

在需要 Docker 的工作之前，适配器校验：

- 引擎可达性与稳定的引擎身份；
- 受支持的 API 行为；
- 所需的镜像摘要；
- cgroup、CPU、内存、pids、OOM、wait 与 inspect 证据；
- 挂载、用户、能力、网络与只读根文件系统的强制执行；
- 看门狗可执行文件与私有控制目录；
- 确定性标签容量与事件可见性。

结果是带类型的结果——compatible、incompatible、unavailable 或 unknown——并附带策略与证据摘要。对于所请求的 profile，不兼容的主机失败关闭。

## 3. 镜像与角色

- cpgen-builder：固定版本的编译器与构建工具。
- cpgen-runtime：用于不受信任目标或 checker 的最小语言运行时。
- cpgen-transfer：受信任的复制辅助程序，用于输入导入与停止后输出导出。

每个镜像引用都是内容摘要。每个容器只有一个角色。传输或守护进程绝不被当作目标来测量。

## 4. 请求契约

~~~text
CompileRequest
  RunID
  AttemptID
  SandboxExecutionID
  LogicalOperationID
  SourceOccurrence
  Language
  ToolchainDigest
  Limits
  OutputDeclaration
  ScopeDigest
  PlanDigest
  EngineIdentityDigest

RunRequest
  RunID
  AttemptID
  SandboxExecutionID
  LogicalOperationID
  ProgramOccurrence
  InputOccurrences
  Argv
  EnvironmentAllowlist
  Limits
  OutputDeclarations
  ScopeDigest
  PlanDigest
  EngineIdentityDigest
~~~

取值严格，会先复制并在授权前校验。命令是显式的 argv 数组。请求不能选择已审计 profile 之外的镜像、挂载、Docker 标志、网络、用户、标签或主机路径。

## 5. 授权身份

密封的授权身份恰好是：

~~~text
RunID
+ AttemptID
+ SandboxExecutionID
+ LogicalOperationID
+ ScopeDigest
+ PlanDigest
+ EngineIdentityDigest
~~~

物理授予只增加 CallID、资源序号与调用角色。前台 CLI 在授权新资源或目标启动之前必须持有按 run 的进程锁。每条数据库命令还会检查预期的 SandboxExecution 或资源生命周期版本。

清理授权更窄：它只能检查、停止、杀死、等待、移除并结算一个确切持久化的身份。它不能启动目标、创建未计划的资源、继续旧的导出，也不能发布制品。

## 6. 完整资源计划

在第一次 Docker create 之前，一个短事务存储：

- SandboxExecution 及其稳定的逻辑身份；
- 完整的、不扩张的资源集；
- 确定性的容器与卷名称；
- 预期的规范标签与调用角色；
- 引擎身份摘要；
- 计划与作用域摘要；
- 截止时间与清理策略；
- 看门狗控制记录摘要；
- 初始生命周期版本。

标签包括 version、RunID、AttemptID、SandboxExecutionID、LogicalOperationID、适用时的 CallID、资源序号、角色、计划摘要与引擎摘要。不包含任何密钥、路径、用户文本或可变进程标识符。

## 7. create 前的看门狗协议

1. 以私有且经认证的本地控制通道启动看门狗。
2. 发送密封的完整计划。
3. 看门狗校验摘要、订阅引擎事件，并执行资源类型基线扫描。
4. 它确认该计划。
5. 在每次 create 之前，Runner 按预期生命周期版本将该资源从 PLANNED 改为 CREATING，并收到针对该资源的 create 前确认。
6. Runner 在数据库事务之外调用 Docker。
7. 它持久化引擎资源 ID 与身份证据。
8. 看门狗确认所发现的确切资源。
9. 只有此时目标才可启动。

该顺序覆盖了 create 返回前死亡、create 返回后但持久化前死亡，以及目标启动前死亡这几种情况。确定性的名称与标签身份允许后续无需大范围扫描即可发现资源。

## 8. 目标直接执行

Runner 创建目标容器时使用：

- 显式 argv 且不使用 shell；
- 非 root 的数字用户；
- 只读根文件系统；
- 丢弃的能力与 no-new-privileges；
- pids、内存、CPU、墙钟、输出与文件大小限制；
- 显式的只读输入与已声明的可写输出；
- 除非单独审计的 profile 要求，否则禁用网络；
- 不挂载 Docker socket 或主机控制目录。

受信任的主机代码负责启动、等待、计时、捕获有上限的 stdout 与 stderr、检查 OOM 与资源证据，并产出 ProcessOutcome。目标判定的优先级是确定性的，且仍由 Judge 契约定义。

## 9. 看门狗行为

分离式看门狗只负责密封计划与停止安全。它在以下情况停止或杀死计划内的目标：

- 执行截止时间；
- 父控制通道 EOF；
- 显式取消或停止命令；
- 看门狗策略违规。

它使用有界的 Stop、Kill、Wait、Inspect 与 Remove 操作，记录证据，并执行最终的确切身份扫描。它会持续存在，直到计划内的调用都已终结、没有目标在运行，且所需的清理证据已结算。

看门狗不能调度阶段、选择 Judge 判定、访问制品存储、继续导出，也不能检查无关的资源。

## 10. 输出传输

目标输出位于计划内的卷上。在证明目标已停止后，一个专用传输容器以只读方式挂载该卷，并且只写入已声明的受信任暂存接收端。制品写入器在提升之前强制执行路径、类型、数量、大小、摘要与预算限制。

CLI 死亡后未完成的导出绝不会被隐式恢复。后续对账会清理旧执行；新的阶段 attempt 可以创建新的逻辑操作。

## 11. 窄范围重启对账

在有状态命令启动时以及当前阶段恢复之前，对账器只加载该 run 的未完成 SandboxExecution 行。对于每个确切计划内的资源，它：

1. 校验引擎身份；
2. 解析确定性名称与规范标签；
3. 在存在时比较持久化的引擎 ID 与身份证据；
4. 检查并证明当前状态；
5. 停止或杀死任何残留的不受信任目标；
6. 等待停止证据；
7. 移除持久化策略允许的资源；
8. 使用预期的生命周期版本结算 CallTrace、预算与沙箱事件；
9. 将该执行标记为 CLEANED，或返回带类型的待清理结果。

完整持久化计划中不存在的资源绝不会被触碰。重复该过程是幂等的。对账器不能修改无关的 run 投影。

## 12. 取消与终态

取消请求会停止对新工作的授权并取消执行上下文。清理使用独立的有界上下文，因此当用户上下文结束时目标停止不会被放弃。

在所有相关的不受信任目标都被证明已停止之前，run 不能提交 CANCELLED、BLOCKED、READY 或任何其他承诺没有存活目标的状态。如果清理超出命令时限，CLI 以文档化的待清理退出码退出，之后的恢复会重复对账。

## 13. ProcessOutcome

结果判定顺序是稳定的：

1. 主机或身份不兼容；
2. 看门狗或 Runner 安全失败；
3. 超时与强制停止；
4. OOM 或资源限制失败；
5. 输出限制失败；
6. 目标被信号终止或以非零码退出；
7. checker 或协议结果；
8. 成功。

每个结果都携带 CallTrace、引擎与镜像身份、计划摘要、目标容器身份、计时、退出与停止证据、stdout/stderr 摘要、资源测量值，以及适用时的制品声明。

## 14. 测试

保留所有 Slice 0 测试，并增加：

- 目标运行期间进程死亡；
- 导出前目标停止；
- 清理期间死亡；
- 看门狗 EOF 与看门狗死亡；
- create 在身份持久化之前返回；
- 通过确切名称与标签进行延迟资源发现；
- 拒绝不匹配的引擎、计划、标签或作用域；
- 拒绝触碰相似的无关容器；
- 可重复的对账；
- 不继续旧的导出；
- 在停止证明之前不进入终态取消；
- SQLite 写事务期间不发生 Docker 或看门狗 IPC。
