# TestPlan、Generator 与数据流水线设计

> 当前样例策略见[执行样例设计](executed-samples.md)。Statement 答案是草稿占位或原始模型证据；正式答案与解释只在独立执行及 Judge 门禁通过后定稿。

## 当前 MVP 与后续设计的分界

下文保留早期完整数据流水线设计；它不是现有功能清单。当前实现以 `internal/domain/data_drafts.go`、`internal/application/data_verifier.go`、`judge_verifier.go` 与 `quality_verifier.go` 为准：

| 项目 | 当前已实现 | 下文保留的后续设计 |
|---|---|---|
| TestPlan | `schema_version`、`effective_seed`、有序 `cases`；每项含 ordinal/kind/purpose/seed，至少两项 small、一项 boundary、一项 stress | groups、exhaustive profiles、逐约束 coverage evidence、非法输入分类 |
| seed | `cpgen.test-case-seed/v1` 域分隔的 SHA-256，输入为 effective seed 与 ordinal，取前 64 位大端值 | 第 3 节按多版本字段做 HMAC 的方案 |
| 数据验收 | 每个生成输入做两次独立执行并比较字节摘要；Validator 检查样例与生成输入 | 系统化非法负例、逐约束语义覆盖分析器 |
| 答案与质量 | Judge 执行 reference，并对 small 及 V3 样例执行 brute；Quality 对固定 checker 做 AC/WA canary 和逐 case 检查 | SPJ、反例最小化、历史反例优先回归、并行生成组 |

当前 `purpose` 与 small/boundary/stress 分类是计划声明，不能证明生成数据确实覆盖了所有自然语言约束或最坏复杂度。固定 checker 的正负 canary 也不能替代 Validator 的非法输入负例验证。验收范围见 [V3 稳定性复验](../evidence/ready-stability-2026-09-22.md)。以下第 2–10 节中的更丰富 DTO、算法及测试目标应按本表区分，不能当作已运行的测试结论。

## 1. 边界

当前 Data Step 产生 `TestPlanV1`、Generator 和 Validator 源码，不产生定向非法输入计划。DockerSandbox 返回执行证据；Data 验证器检查样例与生成输入，Judge 随后产生答案，Package 从已提交证据组装归档。任何模型、Generator 或 Validator 都不能直接发布题包。

当前生成器通过 stdout 产生一个 case，宿主按计划顺序执行，每个 case 重复两次验证字节可重现；当前没有并行生成组。计划包含 4–12 个 case。定向非法输入和并行生成均属于后续设计。

## 2. 后续设计：完整 TestPlan Schema

下列 schema 是扩展目标，不是当前 `TestPlanV1` 的字段定义；当前 DTO 见顶部对照表。

```text
TestPlan
  schema_version, problem_spec_revision, plan_revision
  groups[]
    group_id, purpose, test_count, size_profile
    generator_profile, coverage_requirements[]
    resource_role(normal|maximum|adversarial)
  small_data
    exhaustive_profiles[], random_count, coverage_requirements[]
  invalid_input_classes[]
  seed_scheme, answer_mode(exact|spj)
```

- `group_id` 使用稳定 ASCII 标识；测试 ID 由 Orchestrator 按规范顺序分配，不由模型提供路径。
- `size_profile/generator_profile` 是版本化 DTO，不是自由命令行。字段有整数范围、总规模上限和跨字段约束。
- 每个 `coverage_requirement` 必须映射到一个可机器检查的生成参数或确定性分析器；只有自然语言声明而无证据的标签不能通过 Coverage Gate。
- `maximum` 组必须覆盖用于 Resource Gate 的最大或结构极端实例，不能只把随机参数调大。
- exact/SPJ 决定答案与 checker 流程；MVP 的 TestPlan 必须为 `exact`。

## 3. 后续设计：Seed 派生与确定性

下列 HMAC 方案尚未用于当前 DTO。当前算法为 SHA-256：`"cpgen.test-case-seed/v1\x00"` 后拼接 `uint64(effective_seed)` 与 `uint64(ordinal)` 的各 8 字节大端编码，取摘要前 8 字节按大端解为 seed。版本身份与算法不得静默替换。

GenerationRequest 的 root seed 不直接重复传给所有测试。Orchestrator 使用固定 `seed_scheme` 从以下规范化输入派生每个 `uint64` seed：

```text
HMAC-SHA-256(root_seed_bytes,
  canonical_length_prefixed_tuple(
    workflow_revision, problem_revision, testplan_revision,
    generator_digest, group_id, test_index, purpose))
```

取明确字节序的前 64 位，并在 manifest/来源中保存派生算法版本和最终 seed。相同规范化输入必须得到相同 seed；修改 Generator、TestPlan 或题目 revision 必须产生新测试身份。

Generator 约束：

- 只使用 Runner 传入的显式 seed，不读取系统时间、熵源、PID、文件顺序或网络。
- Go 使用项目固定的 PRNG 实现/版本，不依赖可能跨版本变化的便利 API；C++ 使用项目提供的固定 seed 辅助函数，不使用未固定行为的 `random_device`。
- 排序、map 遍历、浮点格式和区域设置必须稳定；输出统一 UTF-8/LF。
- 生成时保存 generator/toolchain/profile digest、参数 DTO、seed 和输出 digest。

## 4. 后续设计：单测试生成协议

1. Orchestrator 根据 TestPlan 建立稳定 `TestCaseID`、参数 DTO 和派生 seed。
2. MeteredSandbox 运行 Generator，限定 stdout/声明文件、CPU、内存和字节数。
3. `EXITED/0` 且输出安全提升成功后得到 input `PendingArtifact`；其他 ProcessOutcome 回退 Data Agent 或按基础设施分类。
4. 用同一 current Validator 制品验证 input。`VALID` 后，Orchestrator 才在 attempt 提交事务中建立 `test_input_candidate` 实例。
5. `INVALID`/`VALIDATOR_ERROR` 时保留 generator/validator/输入 digest 和日志作为失败证据，但该 Blob 不进入 current 正式测试快照。
6. exact 题运行 current reference solution，得到答案候选；再以该输出同时作为选手输出/答案做 checker 自检，必须 `AC`。
7. input/answer/checker 证据以同一 `TestCaseRevision` 原子接入 current 测试集，禁止出现当前 `.in` 对应旧 `.ans`。

编译结果按源码/toolchain digest 复用一次；每个运行仍有独立 AttemptCallID、资源限制和来源。

## 5. 后续设计：小数据与差分扩展

- exhaustive profile 必须明确有限状态空间和枚举上限；超过上限失败，不静默变随机。
- 随机小数据使用独立派生 seed，并覆盖最小值、最大值附近、退化结构、重复值、连通性等题型标签。
- 每个输入先过 current Validator，再运行 reference/brute，最后通过 current Checker 比较。
- 任一反例成为不可变回归用例，记录发现它的 revision；修复后必须优先重跑所有仍适用的历史反例。
- 反例最小化器只能使用 Validator 和差异谓词，通过类型化 沙箱 调用；不得执行模型返回的脚本。

## 6. 后续设计：Validator 正负例

合法正例来源：样例、小数据、每个正式组和每种边界 profile。非法负例由版本化 mutator 按 `invalid_input_classes` 产生，至少覆盖：

- 缺 token/多 token/非法字符。
- 数值越界、数量字段不一致。
- 结构约束破坏，如重复边、自环、非连通或非法索引（仅题面禁止时）。
- 超过声明总规模。

负例永不进入正式测试集。Validator 对正例必须 `VALID`、对负例必须 `INVALID`；信号、资源失败、未知退出码和 always-accept/always-reject 都是工具故障。每次校验必须携带非 `NONE` 的 `InputOrigin` 与 `ToolOrigin`：SAMPLE 冲突由 Statement + Data/Validator 联合检查，GENERATED_TEST 冲突回 Data Agent；VERIFICATION 的任意 origin 只形成阻塞项，不得修改导入 Blob；DERIVED 中的 IMPORTED 工具也只能由 Agent 产生新 revision，不能原地覆盖。

## 7. 后续设计：确定性与 Coverage Gate

### 确定性

- 在全新容器和工作目录中，用相同 generator digest、参数、seed、toolchain/profile 至少重跑一次。
- 比较完整输出 Blob digest；只比较解析后的 token 不足以证明字节级可复现。
- 失败时记录两次输出和环境证据，Generator 不得进入正式快照或运行缓存。

### 覆盖

- 每个 TestPlan requirement 产生 `CoverageEvidence(requirement_id, test_case_ids, analyzer_version, details)`。
- 参数型 coverage 可由生成参数直接证明；内容型 coverage 必须由版本化、受信任的内置分析器读取输入后证明。MVP 不执行 LLM 生成的 coverage 分析器；未来若支持，必须作为不可信程序进入 DockerSandbox。
- 所有 requirement 至少绑定一个 current、Validator=VALID 的测试；失效/历史测试不能计数。
- Coverage Gate 不以“测试数量很多”替代边界覆盖。

## 8. 后续设计：并发、预算与提交

本节并行组协议为扩展目标；当前实现按第 1 节顺序执行。

- 测试任务可并行运行，但都绑定相同 ProblemSpec/TestPlan/Generator/Validator/Reference revision；汇合时 CAS 拒绝混合 revision。
- 每个 Generator/Validator/Solution/Checker 物理容器分别计入 沙箱 run 预算。
- input、answer、日志和反例受单文件、单测试组、单 run 制品字节限制；超限立即停止并产生 OLE/预算证据。
- 一个测试的 input/answer/current 证据在单事务中提交；测试组只有全部必需测试成功后才成为 current group。
- 并行分支 BLOCKED 时遵循 workflow 的静默优先原则；已经完整提交且 revision 匹配的测试可保留，半完成测试不能进入 current group。

## 9. 后续设计：失效与修复

| 变化/失败 | 动作 |
|---|---|
| TestPlan/Generator revision 变化 | 失效相应生成输入、答案、Coverage/Resource/Package |
| Validator revision 变化 | 所有 current 输入重新验证；Blob 可保留但合法性证据失效 |
| Reference revision 变化 | 保留输入，重生成全部答案并重跑 Differential/Resource |
| Checker revision 变化 | 重判 Sample/Differential/答案自检 |
| Generator `RE/CE` 或产生非法输入 | Data Agent 修订 |
| GENERATED/DERIVED：Validator 拒绝 SAMPLE | Statement + Data/Validator 联合诊断 |
| GENERATED/DERIVED：Validator 拒绝 GENERATED_TEST | Data Agent 修订 generator/validator/TestPlan |
| VERIFICATION：Validator 拒绝 IMPORTED_TEST | 当前 run 记录 blocker 并 FAILED；不得修改导入 Blob |
| 答案自检非 AC | Solution/Checker 路由，保留测试输入 |
| Docker/内置 checker 故障 | Orchestrator `BLOCKED/FAILED`，不让 Agent 猜修复基础设施 |

## 10. 后续设计：测试目标

本节是上述完整方案的测试目标，不是当前测试通过清单。已完成的实际验收见顶部证据链接。

- root seed 到每个 TestCaseID 的固定向量；revision/generator/group/index 任一变化都会改变派生 seed。
- 同 seed 跨两次全新容器得到相同 digest；时间、PID、随机设备和 map 顺序依赖被检测。
- Generator 非零、TLE/MLE/OLE、空输出、超大输出和非法输入均不能进入 current 测试快照。
- Validator 正/负例、always-accept/always-reject 和题面冲突路由。
- RunRelation × InputOrigin 的路由矩阵：verification 失败不调用内容 Agent，只有显式派生 run 可修订。
- reference 更新后 input 保留、answer 与所有依赖证据失效；Validator 更新后全部输入重新验证。
- 并行组不混合 revision，半提交测试不进入 current group。
- Coverage requirement 只引用 current VALID 测试，删除/失效最后一个覆盖测试会使 Gate 失败。
- 答案自检、Differential 反例保存和回归优先重跑。
