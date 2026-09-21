# ADR-0003：编译、进程和判题结果分层

- Status: Accepted
- Date: 2026-08-29

## 背景

testlib validator/checker 使用非零退出码报告正常的非法输入、WA 和 PE。如果 沙箱 把任意非零退出码直接映射为 RE，Judge Harness 将无法得到业务判决。

## 决策

### 三层结果

```text
CompileOutcome = OK | CE | INFRA_ERROR
ProcessOutcome = EXITED | SIGNALED | TLE | MLE | OLE | INFRA_ERROR
```

`RunResult` 始终保留原始 `exit_code` 或信号。Judge Harness 根据 role 解释：

```text
ValidatorOutcome = VALID | INVALID | VALIDATOR_ERROR
CheckerOutcome   = AC | WA | PE | CHECKER_ERROR
SolutionVerdict  = OK | RE | TLE | MLE | OLE
```

### 固定 testlib_v1 映射

工具链清单保存 testlib 摘要、编译宏和 adapter 版本。MVP 默认映射：

| Role | 退出码 | 结果 |
|---|---:|---|
| validator | 0 | `VALID` |
| validator | `FAIL_EXIT_CODE=3` | `INVALID` |
| validator | 其他 | `VALIDATOR_ERROR` |
| checker | `OK_EXIT_CODE=0` | `AC` |
| checker | `WA_EXIT_CODE=1` | `WA` |
| checker | `PE_EXIT_CODE=2` 或 `DIRT_EXIT_CODE=4` | `PE` |
| checker | `FAIL_EXIT_CODE=3` 或未知 | `CHECKER_ERROR` |

信号、TLE、MLE 或 OLE 对 validator/checker 是内容工具错误，可映射为 `VALIDATOR_ERROR/CHECKER_ERROR`。`INFRA_ERROR` 的优先级高于 role adapter：它不生成 Validator/Checker 业务 Outcome，而由 Orchestrator 分类为暂时性 `BLOCKED` 或不可恢复 `FAILED`。MVP 不支持点数/部分判决。

可信 BUILTIN checker 的自检/运行故障仍保留原始 `ProcessOutcome` 和 `CheckerOutcome=CHECKER_ERROR`；路由层使用独立的 `FailureRouteClass=TRUSTED_TOOL_INFRASTRUCTURE` 将其置于内容修复之前，不把它改写成 沙箱/Process `INFRA_ERROR`。

### Go error 边界

- 可分类的 Docker、工具链和 Runner 故障放进 `INFRA_ERROR` 结果。
- Go `error` 只表示上下文取消、协议/序列化破坏或调用方无法分类的内部错误。
- 沙箱 不产生 RE、WA 或 PE。

## 后果

- Judge Harness 必须为题解、generator、validator 和 checker 提供独立 role adapter。
- testlib 映射必须有固定测试向量，未知退出码失败关闭。
- `INFRA_ERROR` 和 `ExecutionInterrupted` 必须在进入 role adapter 前短路，且不得触发内容 Agent 修订。
- Orchestrator 的 `step_deadline/run_budget_deadline` 与程序 `program_hard_deadline -> TLE` 必须使用不同字段记录。
- 业务修复路由必须同时读取 run 关系与制品来源；VERIFICATION run 不得在原 run 内调用内容 Agent 修改导入包。
