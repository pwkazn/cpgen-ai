# CLI 契约

## 1. 原则

- CLI 是 MVP 唯一用户接口。
- 所有命令支持人类输出和 `--json` 机器输出。
- 命令不得接受任意 shell、Docker 参数或未验证文件路径。
- 状态冲突必须失败，不做隐式强制转换。

## 2. 全局参数

```text
cpgen --config <cpgen.yaml> [--json] [--log-level info] <command>
```

- `--config` 必须显式或在当前项目约定位置唯一发现。
- `--json` 时 stdout 只输出一个版本化 JSON 对象，日志写 stderr。
- 密钥永不出现在 `config effective`、错误或 JSON 输出中。

## 3. 配置与诊断

```text
cpgen config validate
cpgen config effective --redact
cpgen doctor
```

`doctor` 检查数据库、目录、Docker 静态 capability/images、LLM 定价/privacy 配置、Similarity health 和公共提交授权。它不创建 Docker canary 容器、不消费 run budget，也不会发送 prompt/题面；动态 execute capability 显示为“最近 run-scoped canary 摘要/需要在 run 中验证”，且不能生成可推进 run 的 observation。输出只展示去密 endpoint class、Policy version 和 data-class allowlist。

## 4. 生成与任务

```text
cpgen generate --request request.yaml
cpgen run list [--state STATE]
cpgen run show <run-id>
cpgen run events <run-id> [--after-version N]
cpgen run resume <run-id>
cpgen run cancel <run-id> --reason "..."
```

`generate` 创建并同步运行任务直到 `READY/BLOCKED/NEEDS_REVIEW/FAILED/CANCELLED`。若 Sandbox cleanup 在命令的有界本地等待内无法完成，则持久化 `RUNNING(mode=QUIESCING)+CleanupPending`、确认 watchdog ACK，并在 fencing 事务写 `OwnerYieldedForCleanup` 后释放 lease，以退出码 10 返回；这不是 `BLOCKED` 或 `FAILED`，后续用 `run resume` 取得更高 epoch、执行 cleanup-only TAKEOVER 并先收敛。旧 operation 不续跑 export；未完成者 ABANDONED 后新建 logical operation。MVP 不提供后台 daemon；进程中断后通过 `run resume` 恢复。

`run resume`：

- 调度前必须取得 run execution lease；若另一个 CLI 持有未过期 lease，返回状态冲突 4。
- `CREATED`：用于 generate 在“已提交 run、尚未开始首个 Step”之间崩溃的恢复；重新校验已保存的配置快照后原子转为 `RUNNING`。
- `BLOCKED`：不要求事先证明依赖已恢复；`retry_after/backoff` 到期且预算/CANCEL/fencing 守卫允许时，取得 lease 后原子进入受限 `RUNNING(mode=PROBING)`，再以正常 AttemptCall 协议探测原 dependency；健康则恢复原 blocked step，仍不可用则收敛调用并回到 `BLOCKED`。
- `NEEDS_REVIEW`：必须已经存在尚未应用的有效 ReviewDecision。
- `RUNNING`：先执行崩溃恢复；QUIESCING 必须先收敛 SandboxExecution，其他遗留 attempt 再按 recovery evidence 重放或转 `ABANDONED`。
- `READY/FAILED/CANCELLED`：拒绝，除非未来 ADR 定义 reopen。

其中 `RUNNING` 恢复仅在旧 lease 过期后允许；不能仅凭另一个 CLI 看见 RUNNING 就废弃活跃 attempt。`RUNNING(mode=PROBING)` 同样累计 active wall、轮询 CANCEL，并在崩溃后按普通 RUNNING recovery 接管。

`run cancel` 使用持久化 control request：若当前命令就是 execution owner，则立即取消其根 context；若另一个 CLI 持有活跃 lease，则请求由 owner 的 heartbeat goroutine确认并完成清理，取消命令可等待确认并显示状态；若 owner 失联，则等待 lease 过期后以更高 epoch 接管并清理。control request 只能导致 `CANCELLED`，不能修改题目 revision、预算或门禁。

## 5. 审核

```text
cpgen review show <run-id>
cpgen review revise <run-id> --step <name> --patch <patch.json> \
  --reviewer <id> --reason "..."
cpgen review retry <run-id> --budget-patch <budget.json> \
  --reviewer <id> --reason "..."
cpgen review waive <run-id> --gate <gate-id> --evidence <sha256> \
  --reviewer <id> --reason "..."
cpgen review reject <run-id> --reviewer <id> --reason "..."
```

### 前置条件

- 仅 `NEEDS_REVIEW` 可创建 revise/retry/waive/reject。
- `revise` patch 必须通过领域 Schema，并显示将失效的下游节点；`--json` 返回 invalidation list。系统按字段路径白名单判定 presentation-only，用户不能通过参数绕过语义失效。
- `retry` budget delta 必须为正或携带新的外部条件 digest。
- `waive` gate 必须在 Policy 中可豁免，evidence/revision/policy 必须匹配当前快照。
- `--evidence` 可重复；CLI 规范化为排序去重的 digest 数组。证据较多时可使用版本化 evidence manifest 文件。
- reviewer/reason 必填；MVP reviewer 是本地字符串身份，不是权限系统。
- 所有命令（包括 reject）只创建一个 `PENDING` ReviewDecision；创建后仍需 `run resume` 原子应用，便于审计、lease fencing 和自动化明确分步。
- review 命令在暂停态获取短期 lease，CAS 写入决定后立刻释放；若 resume 已经取得 lease，则返回状态冲突，不能与应用流程交错。
- review 写事务同时检查不存在 active CANCEL；取消先提交则 review 返回状态冲突，取消后所有未应用决定转为 STALE。

## 6. 验证与题包

```text
cpgen verify <package-path> [--profile mvp|release]
cpgen package show <package-path>
cpgen package derive <package-path> --target polygon
```

- `verify` 先持久化带独立预算的 `CREATED` verification run，取得 lease 并重新校验配置快照后原子转为 `RUNNING`；只有此后才能创建 reservation/import Blob。随后以不修改源目录的 import 模式执行 PackageStructuralGate：校验与复制使用同一安全文件句柄，经该 run 的 MeteredArtifactSink 把所有后续需要的内容固定为 Blob；任何代码只从这些 Blob 编译/运行，绝不从 observed path 执行。然后按所选 profile 重跑 Schema、Similarity、Compile、Sample、Differential、Validator、Determinism、Coverage、Resource 和 Package Gates；最终 Package Gate 从这些 Blob 重建的私有只读 staging 重验，不回读 observed path。仅当版本化 workflow 明确定义某门禁对该题型不适用时才不调度该 Gate，并在最终报告记录适用性理由。本次 PrePackage evidence 保存在包外并由 receipt 绑定。它为同一 package ID 建立新的 package occurrence、receipt 和最终 QualityReport，不篡改原目录或生成证据。缺少重验所需源码、TestPlan/seed 或其他必需元数据时失败关闭；用户若要补充内容必须显式创建 derived package，不能复用包内自述报告假装已重验。
- GenerationRequest 中的 export targets 由 Orchestrator 在原 run 的 package step 执行，不暴露绕过工作流的手工命令。
- `package derive` 对 READY package 创建新的 derived run、不可变 package ID 和 package occurrence，不修改原目录、manifest 或 run。
- VERIFICATION run 的任意内容门禁失败只记录 blocker 并使该 run FAILED，不调用 Statement/Solution/Data/SPJ Agent。CLI 必须提示用户显式执行 `package derive`；不得在原 verification run 中应用内容 revision。
- Package Gate 失败返回非零并保留 staging diagnostics，不发布正式目录。

## 7. 退出码

| Code | 含义 |
|---:|---|
| 0 | 命令成功；generate/verify 达到 READY |
| 2 | CLI 用法或配置校验错误 |
| 3 | 目标 run/package 不存在 |
| 4 | 状态前置条件冲突或 CAS 冲突 |
| 5 | 任务进入 `BLOCKED` |
| 6 | 任务进入 `NEEDS_REVIEW` |
| 7 | 任务 `FAILED` |
| 8 | 任务 `CANCELLED` 或命令 context 被用户取消 |
| 9 | 未分类内部错误 |
| 10 | run 仍为 `RUNNING(mode=QUIESCING)`，存在持久化 CleanupPending，需要稍后 resume |

CE/WA 等程序 verdict 不直接成为 CLI exit code；它们通过任务状态和 JSON evidence 返回。

## 8. JSON envelope

```json
{
  "schema_version": "cpgen.cli/v1",
  "command": "run.show",
  "ok": true,
  "run_id": "...",
  "run_state": "BLOCKED",
  "run_version": 17,
  "data": {},
  "errors": []
}
```

错误包含稳定 `code`、用户可读 message 和 evidence digest，不包含秘密或未截断第三方响应。

## 9. 验收

- 所有状态前置条件都有 table-driven test。
- `--json` stdout 可被严格 Schema 解析且不混入日志。
- waiver 在 evidence/revision 改变后被拒绝。
- NEEDS_REVIEW 的空操作 resume/retry 被拒绝；BLOCKED resume 可在 retry_after/backoff 到期后发起受计量 probe。
- Ctrl+C 取消在途容器并返回 8，不误报程序 TLE。
