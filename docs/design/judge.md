# Judge Harness 与 testlib 详细设计

当前 MVP 使用固定 exact-token checker，以及 `internal/application` 中的 Solution/Data/Judge/Quality 验证器。下文同时保留早期角色协议与扩展门禁设计；SPJ、反例最小化、通用 coverage 分析和系统化 Validator 非法负例尚不属于已验收能力。`SampleExecutionSmokeCheck` 为早期设计名称，不能据此推断当前存在同名实现。当前 V3 样例流程见第 5.1 节；数据计划的实际字段与能力边界见[数据流水线实现分界](data-pipeline.md#当前-mvp-与后续设计的分界)。

## 1. 职责

Judge Harness 位于沙箱之上，负责：

- 解释 CompileOutcome 与 ProcessOutcome。
- 按 solution/generator/validator/checker 角色映射业务结果。
- 执行 Sample、Differential、Validator、Resource 和 Checker QA 门禁。
- 生成可追溯 QualityCheck，不修改题目内容。

## 2. Go 类型

```go
type CompileOutcome string // OK, CE, INFRA_ERROR
type ProcessOutcome string // EXITED, SIGNALED, TLE, MLE, OLE, INFRA_ERROR

type RunResult struct {
	CallTrace CallTrace
	Outcome   ProcessOutcome
	ExitCode *int
	Signal    *string
	Metrics   ProcessMetrics
	Stdout    *PendingArtifact
	Stderr    *PendingArtifact
	Details   map[string]string
}

type ValidatorOutcome string // VALID, INVALID, VALIDATOR_ERROR
type CheckerOutcome string   // AC, WA, PE, CHECKER_ERROR
type SolutionVerdict string  // OK, RE, TLE, MLE, OLE
type FailureRouteClass string // SANDBOX_INFRA, TRUSTED_TOOL_INFRASTRUCTURE, VERIFICATION_CONTENT, GENERATED_REPAIR
```

所有枚举反序列化时拒绝未知值。`CompileResult` 同样包含公共 `CallTrace`；MeteredSandbox 必须用统一校验器把逻辑操作、物理调用、结果调用/缓存 pin 与当前预授权计划交叉核对后才交给 Judge。

## 3. 角色适配器

### Solution/Brute/Generator

| 进程 | 映射 |
|---|---|
| `EXITED + 0` | `OK` |
| `EXITED + non-zero` | `RE` |
| `SIGNALED` | `RE`，保留信号 |
| `TLE/MLE/OLE` | 同名判定 |
| `INFRA_ERROR` | 不产生程序判定，任务 `BLOCKED/FAILED` |

### testlib_v1 Validator

| 进程 | 映射 |
|---|---|
| `EXITED + 0` | `VALID` |
| `EXITED + FAIL_EXIT_CODE(3)` | `INVALID` |
| `EXITED + other` | `VALIDATOR_ERROR` |
| `SIGNALED/TLE/MLE/OLE` | `VALIDATOR_ERROR`，同时保存底层结果 |
| `INFRA_ERROR` | 不生成 `ValidatorOutcome`；原样交给 Orchestrator |

`INVALID` 仅表示 validator 按协议拒绝输入。Quality Gate 结合样例类型判断该拒绝是预期还是故障：正式/合法正例期待 `VALID`，定向非法负例期待 `INVALID`。

### testlib_v1 Checker

| 进程 | 映射 |
|---|---|
| `EXITED + OK_EXIT_CODE(0)` | `AC` |
| `EXITED + WA_EXIT_CODE(1)` | `WA` |
| `EXITED + PE_EXIT_CODE(2)` | `PE` |
| `EXITED + DIRT_EXIT_CODE(4)` | `PE`，reason=`DIRT` |
| `EXITED + FAIL_EXIT_CODE(3)` | `CHECKER_ERROR` |
| `EXITED + unknown` | `CHECKER_ERROR` |
| `SIGNALED/TLE/MLE/OLE` | `CHECKER_ERROR`，同时保存底层结果 |
| `INFRA_ERROR` | 不生成 `CheckerOutcome`；原样交给 Orchestrator |

testlib 可通过宏修改退出码，因此映射来自 `ToolchainManifest.TestlibAdapter`，不能散落硬编码。MVP 工具链禁止自定义这些宏并固定适配器版本。

所有角色适配器共用同一条前置规则：`ExecutionInterrupted` 和 `CompileOutcome/ProcessOutcome=INFRA_ERROR` 的优先级高于角色映射。前者按 context 原因交回工作流；后者由 Orchestrator 分类为暂时性 `BLOCKED` 或不可恢复 `FAILED`。两者都不得产生 Validator/Checker 业务结果、内容修复请求或新的 Agent revision。

## 4. 工具链 manifest

```json
{
  "toolchain_id": "cpp20-testlib-v1",
  "builder_image_digest": "sha256:...",
  "compiler": "g++ ...",
  "testlib_digest": "sha256:...",
  "adapter": "testlib_v1",
  "exit_codes": {"ok": 0, "wa": 1, "pe": 2, "fail": 3, "dirt": 4},
  "partial_supported": false
}
```

manifest 摘要进入编译缓存、运行来源和 Package manifest。

## 5. 门禁流程

### 5.1 Sample Gate

当前 V3 的样例权威来源以[执行样例设计](executed-samples.md)为准：`solution_verify` 对草稿样例执行 std/brute 并核对独立物理调用与 token 一致；Data 验证输入合法；Judge 再对样例和 generated-small 做差分，并核对样例跨阶段输出一致，全部通过后才定稿。Quality 继续检查固定 checker 与每个 case。模型草稿答案不参与 V3 的比较。

下述 smoke 检查与“比较题面样例输出”的流程保留为历史切片/声明样例策略说明，不代表当前 V3 的 oracle 来源；历史 run 仍按自己的工作流修订读取证据。

正式 Sample Gate 的调度前置条件是当前 reference solution、当前 Validator 和 checker 均已编译，并且三者与同一个 ProblemSpec revision/输入摘要绑定。Slice 3 的 `SampleExecutionSmokeCheck` 只验证程序可启动和固定 checker 路径，不生成正式 Gate 证据；正式 Sample Gate 位于 Slice 4。

`SampleExecutionSmokeCheck` 的输入与判定固定如下：框架先用可信 A+B fixture 对 BUILTIN checker 做 AC/WA 自检；再把 ProblemSpec 声明的样例输入作为 `InputOrigin=SAMPLE` 分别启动 reference/brute，只要求沙箱成功、进程 `EXITED(0)` 且输出未超限，不调用尚未生成的 Validator，也不把输出与题面期望输出比较。checker 自检失败是基础设施故障；生成程序 CE/RE/TLE/MLE/OLE 或样例无法解析是 `SAMPLE_SMOKE_PROCESS_ERROR`，由 Statement + Solution/Brute 联合诊断。任何 smoke 结果都不是 Sample Gate/正确性证据。

1. 样例输入先通过 Validator。
2. 标程执行成功。
3. exact 题用默认 Checker 比较实际输出与题面样例输出。
4. 任一 WA/PE 产生修复证据，回退 Statement/Solution。

### 5.2 Differential Gate

对每个枚举/随机小输入：

1. Validator 必须返回 `VALID`。
2. 标程和暴力均为 `OK`。
3. exact 题：把暴力输出作为 answer，使用配置 Checker 判断标程输出；需要对称验证时再交换。
4. Phase 2 SPJ：分别验证两个输出合法，若题目有目标值再比较目标值。
5. 发现差异保存输入、两个输出、程序 digest、checker digest 和 seed；可选执行反例最小化。

不得使用原始字符串相等替代 Checker。

### 5.3 Validator Gate

- 所有样例、小数据和正式测试必须 `VALID`。
- 每种约束至少生成一个明确非法负例并期待 `INVALID`。
- Validator 总是拒绝会在合法正例上失败；总是接受会在非法负例上失败。
- Validator 未知退出码、信号、TLE/MLE/OLE 均为内容工具错误，回退 Data Agent；沙箱 `INFRA_ERROR` 不进入该路由。

### 5.4 Resource Gate

- 只在正确性 Gate 通过后运行。
- 使用最大/极端测试组，不读取普通 run cache 的性能数据。
- 采用所选 verification profile 的 CPU、内存和输出裕量。
- 记录宿主/内核/Docker/profile/toolchain 指纹。
- CPU 时间可判定是不可豁免的 TLE 前提：正式 Resource Gate 只接受带 `CPUTimeLimit`、非空权威 `cpu_time_ms` 及对应测量证据的运行记录。Docker `--cpus` 仅为 CPU 配额，不能单独满足此条件；墙钟超时仅用于安全停止。
- `mvp` 可保留为编译、样例和 Differential Gate 的功能性执行 profile，但 `cpu_time_ms=null` 时必须使 Resource Gate `BLOCKED(capability_missing=cpu_time_measurement)`，不得将墙钟超时映射为题目 CPU-TLE 或提交为最终性能验证。`release` 使用独立验证 run，并提供 CPU-time measurement capability canary。

### 5.5 Checker QA

MVP 默认 Checker 使用固定实现，至少测试：

- 标程答案 → AC。
- 明确错误值 → WA。
- 多余/污染输出 → WA 或 PE（按 checker 契约固定）。
- checker 自己的 FAIL/未知退出码 → CHECKER_ERROR。

Phase 2 生成 SPJ 时增加合法异解、越界、重复、缺项和 checker 攻击用例。

## 6. 修复路由

路由输入必须包含 `RunRelation(GENERATED|DERIVED|VERIFICATION)`、`InputOrigin(NONE|SAMPLE|GENERATED_TEST|IMPORTED_TEST|TRUSTED_FIXTURE)`、`ToolRole(SOLUTION|BRUTE|GENERATOR|VALIDATOR|CHECKER)`、`ToolOrigin(BUILTIN|GENERATED|IMPORTED)`、`OutcomeCategory(COMPILE_FAILURE|PROGRAM_TOOL_ERROR|SEMANTIC_ACCEPTANCE_ERROR|INFRA_ERROR)` 和适用时的 `CheckerScenario(SAMPLE_SMOKE|SAMPLE_EXPECTED|DIFFERENTIAL|ANSWER_SELF_CHECK)`。`NONE` 只用于不依赖输入的 Compile，`TRUSTED_FIXTURE` 只用于可信 checker 自检；所有程序/语义输入必须使用 SAMPLE/GENERATED_TEST/IMPORTED_TEST。路由层另有 `FailureRouteClass(SANDBOX_INFRA|TRUSTED_TOOL_INFRASTRUCTURE|VERIFICATION_CONTENT|GENERATED_REPAIR)`；优先级固定为沙箱 `INFRA_ERROR` 或 `TRUSTED_TOOL_INFRASTRUCTURE`（可信 BUILTIN checker 故障） > VERIFICATION 内容失败 > GENERATED/DERIVED 修复。它不改写三层 Judge 结果中的原始 `ProcessOutcome`/`CheckerOutcome=CHECKER_ERROR`。在最后一层中，按场景特定的行先于通用角色行：

表格匹配先应用上一段的全局优先级，再在同一层按场景特定优先于通用的规则；不得按下方行的书写顺序覆盖该优先级。

| Run relation / 场景 | 路由 |
|---|---|
| GENERATED/DERIVED：solution/brute CE/RE/TLE/MLE/OLE | Solution/Brute Agent |
| GENERATED/DERIVED：SAMPLE_SMOKE_PROCESS_ERROR | Statement + Solution/Brute 联合诊断；不产生 Gate 证据 |
| GENERATED/DERIVED：SAMPLE 被 Validator 拒绝 | Statement + Data/Validator 联合诊断样例、约束与 validator |
| GENERATED/DERIVED：GENERATED_TEST 被拒绝、GENERATED/IMPORTED Validator CE 或 signal/TLE/MLE/OLE/未知码 | Data Agent 产生新 revision；不覆盖 imported Blob |
| GENERATED/DERIVED：SAMPLE_EXPECTED WA/PE | Statement/Solution |
| GENERATED/DERIVED：DIFFERENTIAL WA/PE | Solution/Brute；保存反例 |
| GENERATED/DERIVED：GENERATED/IMPORTED Checker 在任意 scenario 中 CE、signal/TLE/MLE/OLE/未知码或语义 QA 失败 | SPJ Agent 产生新 revision；不覆盖 imported Blob |
| 任意 relation：BUILTIN checker 自检/运行失败（`TRUSTED_FIXTURE`） | 保留 `ProcessOutcome` 与 `CheckerOutcome=CHECKER_ERROR`，并标记 `FailureRouteClass=TRUSTED_TOOL_INFRASTRUCTURE`；走 Orchestrator 基础设施路径，不调用 SPJ Agent |
| VERIFICATION：任意内容 Gate 失败 | 记录 blocker 并使当前验证 run FAILED；禁止修改 IMPORTED_TEST 或调用内容 Agent |
| 任意 relation：沙箱 INFRA_ERROR | 在所有内容路由前短路到 Orchestrator；暂时性 BLOCKED、不可恢复 FAILED，不创建内容 revision |

用户希望修复验证失败的外部包时，只能显式 `package derive` 创建新的 DERIVED run；原验证 run 与导入题包保持不可变。

## 7. QualityCheck

```text
QualityCheck
  gate_id, gate_version, status(PASSED|FAILED|WAIVED|BLOCKED)
  input_revision, policy_version, evidence_digests[]
  metrics, environment_digest?, waiver_id?, created_at
```

不可豁免 Gate 不允许状态 `WAIVED`。同 Gate 新 revision 通过后，旧记录保留但不进入当前 QualityReport。

## 8. 测试向量

- solution：exit 0、exit 42、SIGSEGV、TLE、MLE、OLE。
- validator：0、3、未知 9、信号、TLE。
- checker：0、1、2、4、3、未知 9、信号、TLE。
- Orchestrator 取消与程序 TLE 同时触发时，验证主结果和清理证据。
- testlib manifest 摘要改变后，旧适配器/缓存不得复用。
