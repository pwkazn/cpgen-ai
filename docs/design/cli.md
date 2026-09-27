# CLI 契约

状态：CLI 契约；ADR-0006 的 2026-09-26 修订另增本地 Web 入口

## 1. 原则

CLI 保持本地有界命令接口。`serve` 是同一二进制的前台本地 Web 工作台入口；它绑定 loopback 并在进程内承载任务 goroutine，不启动每任务 CLI 子进程。不存在远程控制端点。

状态命令默认将 JSON 信封写入 stdout，进度写入 stderr。help、version 与 doctor 是独立入口；version 可选 `--json`，doctor 要求 `--json`。密钥以及原始私有 prompt 或源码内容永不记入日志。

## 2. 全局选项

~~~text
--config PATH
~~~

状态命令使用 `cpgen --config PATH <command>`，下文省略此前缀。help、version 和 doctor 不接受 `--config`。当前没有全局 `--workspace`、`--json`、`--quiet` 或 `--log-level`。路径在使用前会先规范化。运行时目录、数据库、锁、制品、staging 与看门狗控制必须保持私有。

## 3. 配置与诊断

~~~text
cpgen --config PATH config validate
cpgen --config PATH config effective --redact
cpgen --config PATH serve [--listen 127.0.0.1:8080]
cpgen doctor --json --engine-endpoint ENDPOINT --api-version VERSION --builder-image SHA256 --runtime-image SHA256 --transfer-image SHA256 --execution-protocol docker-direct-v2
~~~

config validate 校验配置，不读取工具链锁或联系外部服务；effective 要求显式 `--redact`。doctor 检查 Docker 与工具链能力，不检查模型或查重提供方，也不持久化工作流进度。

serve 在启动时校验配置并只绑定 loopback。静态页面随二进制嵌入；浏览器会话为本地随机值，配置摘要只读。Web 不提供 config validate、doctor 或诊断执行端点。实际生成与恢复仍由 application 按 run 冻结策略检查执行依赖。

## 4. generate 与 run 命令

~~~text
cpgen generate --request request.yaml
cpgen run list [--state STATE] [--limit N]
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
cpgen review revise <run-id> --reviewer NAME --step STEP --patch FILE --reason TEXT
cpgen review retry <run-id> --reviewer NAME [--budget-patch FILE] [--evidence DIGEST] --reason TEXT
cpgen review waive <run-id> --reviewer NAME --gate DIGEST --evidence DIGEST --reason TEXT
cpgen review reject <run-id> --reviewer NAME --reason TEXT
~~~

会改变状态的复核命令仅对 NEEDS_REVIEW 有效，它创建一个不可变的 PENDING ReviewDecision，绑定到期望的 run 版本、工作流 revision、当前阶段输入、证据与策略。它不会直接继续 run。用户随后执行 run resume。

retry 至少需要 `--budget-patch` 或 `--evidence` 之一。waive 仍受阶段可豁免策略限制，不能越过不可豁免的查重或质量门禁。show 呈现当前复核状态与决定。

## 6. 题包命令

~~~text
cpgen --config PATH run export <run-id> --output PATH
~~~

export 要求 READY 和当前已校验的题包 occurrence，重新核验已提交证明后原子发布，拒绝覆盖目标；目标父目录必须存在并支持硬链接。新 run 的工具链快照允许原锁文件丢失后的离线导出。独立 ZIP 编译执行由验收测试提供；`package verify`、`package export`、Polygon 导出和 `gc` 尚不是公开 CLI 命令。制品维护仅提供 application API。

## 7. 重启语义

- CREATED 启动第一个编译后的阶段。
- RUNNING 表示先前的命令可能已退出；resume 依据持久化证据核对当前尝试。
- BLOCKED 启动同一阶段的新尝试，并重新校验其精确依赖。
- NEEDS_REVIEW 需要一个适用的待决决定。
- READY、FAILED 与 CANCELLED 的 resume 不重启生成；CLI 返回现有投影及对应状态退出码。
- 未来的 retry-after 时间会返回类型化的阻塞结果；不会有后台计时器等待它。
- 未知的外部发送边界绝不会以新的幂等键重发。

## 8. 退出码

| 代码 | 含义 |
|---:|---|
| 0 | 成功，包括 READY 或成功的只读命令 |
| 2 | 参数、请求或配置非法 |
| 3 | run 或对象不存在（`not_found`） |
| 4 | run 进程锁冲突（`lock_busy`） |
| 5 | BLOCKED，或操作的 `invalid_state` |
| 6 | NEEDS_REVIEW |
| 7 | FAILED，或操作的 `version_conflict` |
| 8 | CANCELLED |
| 9 | 其他操作失败（通常为 `operation_failed`） |
| 10 | 安全清理仍待完成；run 未进入终态 |

状态退出码用于 generate/resume 的结果；成功的读取、复核提交和 cancel 返回 0，具体状态从 JSON 读取。结合 `status` 与 `error.code` 区分业务结果和操作失败。退出码 10 表示后续 resume 必须重复精确资源清理。doctor 使用独立输出和能力结果，不套用本表中的业务状态。

## 9. JSON 信封

~~~json
{
  "schema_version": "cpgen.cli/v1",
  "status": "ERROR",
  "error": {
    "code": "not_found",
    "message": "run not found"
  }
}
~~~

成功结果位于 `data`，run 状态结果另含 `run_version`；错误位于 `error`，配置错误可含 `field`。当前信封不提供 `command`、`ok`、`result` 或 `retryable` 字段。实现与参数解析见 `internal/cli/command.go`、`internal/cli/run.go`。

## 10. 验收

子进程测试验证锁冲突、进程死亡与 resume、活跃期间 cancel、稳定的 JSON、退出码映射、无密钥泄漏，以及 JSON 模式下 stdout 不出现进度文本。
