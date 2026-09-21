# CLI 契约

状态：ADR-0006 下的现行设计

## 1. 原则

CLI 是 Phase 1 唯一的接口。命令打开本地资源，执行有界的前台工作，打印稳定输出，然后退出。不存在后台工作流进程或远程控制端点。

人类可读输出中，进度写入 stderr，结果写入 stdout。JSON 模式输出一个带版本的信封，不含任何装饰性文本。密钥以及原始私有 prompt 或源码内容永不记入日志。

## 2. 全局选项

~~~text
--config PATH
--workspace PATH
--json
--quiet
--log-level LEVEL
~~~

路径在使用前会先规范化。运行时目录、数据库、锁、制品、staging 与看门狗控制必须保持私有。

## 3. 配置与诊断

~~~text
cpgen config check [--request request.yaml]
cpgen doctor [--docker] [--providers]
~~~

config check 解析、合并、规范化、脱敏并校验配置，且不改变 run 状态。doctor 报告类型化的能力结果，不持久化工作流进度。

## 4. generate 与 run 命令

~~~text
cpgen generate --request request.yaml
cpgen run list [--state STATE]
cpgen run show <run-id>
cpgen run events <run-id> [--after-version N]
cpgen run resume <run-id>
cpgen run cancel <run-id> --reason "..."
~~~

generate 创建 run 并执行编译后的流水线，直到 READY、BLOCKED、NEEDS_REVIEW、FAILED 或 CANCELLED。

run list、show 与 events 是只读的，不获取 run 执行锁。events 按 run 版本排序，并支持稳定分页。

run resume 是唯一常规的重启命令。它获取确定性的每 run 进程锁，核对精确的未完成沙箱资源，并恢复当前编译后的阶段。如果先前进程在某个尝试期间退出，resume 会先依据已持久化的领域证据记录或重放该尝试，然后才创建新工作。

run cancel 插入一个幂等的取消请求。当执行器活跃时，它会观察到该请求并停止。当没有执行器持有 run 锁时，cancel 可以获取该锁，并在提交 CANCELLED 之前核对精确的沙箱资源。

### 进程锁冲突

如果 generate 或 resume 发现同一 run 已被锁定，它会返回退出码 4 以及包含 RunID 与 operation 的类型化 StateConflict。它不会无限等待、不会改变状态，也不会启动阶段。不同的 run 相互独立。

## 5. 复核命令

~~~text
cpgen review show <run-id>
cpgen review revise <run-id> --patch FILE --reason TEXT
cpgen review retry <run-id> --reason TEXT
cpgen review waive <run-id> --policy RULE --reason TEXT
cpgen review reject <run-id> --reason TEXT
~~~

会改变状态的复核命令仅对 NEEDS_REVIEW 有效，它创建一个不可变的 PENDING ReviewDecision，绑定到期望的 run 版本、工作流 revision、当前阶段输入、证据与策略。它不会直接继续 run。用户随后执行 run resume。

show 呈现当前复核 checkpoint、待决决定、预算与所引用的证据。

## 6. 题包命令

~~~text
cpgen package verify PATH
cpgen package export <run-id> --format internal|polygon --output PATH
cpgen gc
~~~

verify 将输入视为不可信，执行结构与语义检查，且不修改 run。export 要求存在已校验的题包 occurrence，并通过私有 staging 目录写入，最后原子发布。gc 是显式的维护操作，会获取独占的制品锁。

## 7. 重启语义

- CREATED 启动第一个编译后的阶段。
- RUNNING 表示先前的命令可能已退出；resume 依据持久化证据核对当前尝试。
- BLOCKED 启动同一阶段的新尝试，并重新校验其精确依赖。
- NEEDS_REVIEW 需要一个适用的待决决定。
- READY、FAILED 与 CANCELLED 拒绝 resume。
- 未来的 retry-after 时间会返回类型化的阻塞结果；不会有后台计时器等待它。
- 未知的外部发送边界绝不会以新的幂等键重发。

## 8. 退出码

| 代码 | 含义 |
|---:|---|
| 0 | 成功，包括 READY 或成功的只读命令 |
| 2 | 参数、请求、配置或输入题包非法 |
| 3 | BLOCKED |
| 4 | 状态、版本、复核或进程锁冲突 |
| 5 | NEEDS_REVIEW |
| 6 | FAILED |
| 7 | CANCELLED |
| 8 | 预算耗尽 |
| 9 | 沙箱或主机能力不兼容 |
| 10 | 安全清理仍待完成；run 未进入终态 |
| 70 | 意外的内部错误 |

退出码 10 表示后续 resume 必须重复精确资源清理。CLI 会打印 run ID 与当前持久化状态，而不会声称已完成。

## 9. JSON 信封

~~~json
{
  "schema_version": 1,
  "command": "run.resume",
  "ok": false,
  "run_id": "run_...",
  "state": "BLOCKED",
  "run_version": 12,
  "result": {},
  "error": {
    "code": "dependency_unavailable",
    "message": "sanitized message",
    "retryable": true
  }
}
~~~

在同一版本内，schema 是向后追加的。ID、状态、事件版本、决定 ID、预算摘要与题包 occurrence ID 都是稳定的机器字段。

## 10. 验收

子进程测试验证锁冲突、进程死亡与 resume、活跃期间 cancel、稳定的 JSON、退出码映射、无密钥泄漏，以及 JSON 模式下 stdout 不出现进度文本。
