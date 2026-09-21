# TestPlan、Generator 与数据流水线设计

## 1. 边界

Data Step 产生结构化 `TestPlan`、Generator 源码、Validator 源码和定向非法输入计划。DockerSandbox 只运行程序并返回 `PendingArtifact`；Judge Harness 解释 Validator 结果；Orchestrator 决定哪些 Blob 成为 current 测试集。任何 Agent、Generator 或 Validator 都不能直接写 `tests/`、题包或 ArtifactOccurrence。

MVP 每次 Generator 调用只生成一个声明的测试文件到 stdout 或固定输出路径。批量测试由 Orchestrator 多次调用并行实现，避免模型控制目录遍历或动态文件清单。

## 2. TestPlan Schema

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

## 3. Seed 派生与确定性

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

## 4. 单测试生成协议

1. Orchestrator 根据 TestPlan 建立稳定 `TestCaseID`、参数 DTO 和派生 seed。
2. MeteredSandbox 运行 Generator，限定 stdout/声明文件、CPU、内存和字节数。
3. `EXITED/0` 且输出安全提升成功后得到 input `PendingArtifact`；其他 ProcessOutcome 回退 Data Agent 或按基础设施分类。
4. 用同一 current Validator 制品验证 input。`VALID` 后，Orchestrator 才在 attempt 提交事务中建立 `test_input_candidate` 实例。
5. `INVALID`/`VALIDATOR_ERROR` 时保留 generator/validator/输入 digest 和日志作为失败证据，但该 Blob 不进入 current 正式测试快照。
6. exact 题运行 current reference solution，得到答案候选；再以该输出同时作为选手输出/答案做 checker 自检，必须 `AC`。
7. input/answer/checker 证据以同一 `TestCaseRevision` 原子接入 current 测试集，禁止出现当前 `.in` 对应旧 `.ans`。

编译结果按源码/toolchain digest 复用一次；每个运行仍有独立 AttemptCallID、资源限制和来源。

## 5. 小数据与差分

- exhaustive profile 必须明确有限状态空间和枚举上限；超过上限失败，不静默变随机。
- 随机小数据使用独立派生 seed，并覆盖最小值、最大值附近、退化结构、重复值、连通性等题型标签。
- 每个输入先过 current Validator，再运行 reference/brute，最后通过 current Checker 比较。
- 任一反例成为不可变回归用例，记录发现它的 revision；修复后必须优先重跑所有仍适用的历史反例。
- 反例最小化器只能使用 Validator 和差异谓词，通过类型化 沙箱 调用；不得执行模型返回的脚本。

## 6. Validator 正负例

合法正例来源：样例、小数据、每个正式组和每种边界 profile。非法负例由版本化 mutator 按 `invalid_input_classes` 产生，至少覆盖：

- 缺 token/多 token/非法字符。
- 数值越界、数量字段不一致。
- 结构约束破坏，如重复边、自环、非连通或非法索引（仅题面禁止时）。
- 超过声明总规模。

负例永不进入正式测试集。Validator 对正例必须 `VALID`、对负例必须 `INVALID`；信号、资源失败、未知退出码和 always-accept/always-reject 都是工具故障。每次校验必须携带非 `NONE` 的 `InputOrigin` 与 `ToolOrigin`：SAMPLE 冲突由 Statement + Data/Validator 联合检查，GENERATED_TEST 冲突回 Data Agent；VERIFICATION 的任意 origin 只形成阻塞项，不得修改导入 Blob；DERIVED 中的 IMPORTED 工具也只能由 Agent 产生新 revision，不能原地覆盖。

## 7. 确定性与 Coverage Gate

### 确定性

- 在全新容器和工作目录中，用相同 generator digest、参数、seed、toolchain/profile 至少重跑一次。
- 比较完整输出 Blob digest；只比较解析后的 token 不足以证明字节级可复现。
- 失败时记录两次输出和环境证据，Generator 不得进入正式快照或运行缓存。

### 覆盖

- 每个 TestPlan requirement 产生 `CoverageEvidence(requirement_id, test_case_ids, analyzer_version, details)`。
- 参数型 coverage 可由生成参数直接证明；内容型 coverage 必须由版本化、受信任的内置分析器读取输入后证明。MVP 不执行 LLM 生成的 coverage 分析器；未来若支持，必须作为不可信程序进入 DockerSandbox。
- 所有 requirement 至少绑定一个 current、Validator=VALID 的测试；失效/历史测试不能计数。
- Coverage Gate 不以“测试数量很多”替代边界覆盖。

## 8. 并发、预算与提交

- 测试任务可并行运行，但都绑定相同 ProblemSpec/TestPlan/Generator/Validator/Reference revision；汇合时 CAS 拒绝混合 revision。
- 每个 Generator/Validator/Solution/Checker 物理容器分别计入 沙箱 run 预算。
- input、answer、日志和反例受单文件、单测试组、单 run 制品字节限制；超限立即停止并产生 OLE/预算证据。
- 一个测试的 input/answer/current 证据在单事务中提交；测试组只有全部必需测试成功后才成为 current group。
- 并行分支 BLOCKED 时遵循 workflow 的静默优先原则；已经完整提交且 revision 匹配的测试可保留，半完成测试不能进入 current group。

## 9. 失效与修复

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

## 10. 测试

- root seed 到每个 TestCaseID 的固定向量；revision/generator/group/index 任一变化都会改变派生 seed。
- 同 seed 跨两次全新容器得到相同 digest；时间、PID、随机设备和 map 顺序依赖被检测。
- Generator 非零、TLE/MLE/OLE、空输出、超大输出和非法输入均不能进入 current 测试快照。
- Validator 正/负例、always-accept/always-reject 和题面冲突路由。
- RunRelation × InputOrigin 的路由矩阵：verification 失败不调用内容 Agent，只有显式派生 run 可修订。
- reference 更新后 input 保留、answer 与所有依赖证据失效；Validator 更新后全部输入重新验证。
- 并行组不混合 revision，半提交测试不进入 current group。
- Coverage requirement 只引用 current VALID 测试，删除/失效最后一个覆盖测试会使 Gate 失败。
- 答案自检、Differential 反例保存和回归优先重跑。
