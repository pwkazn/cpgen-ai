# 先完成可用出题闭环

状态：2026-09-09 用户调整后的当前开发优先级。此计划取代此前以变异机制为前置条件的开发顺序。LOOP-01 / SOL-01 已完成至公开 CLI 的编译/样例验证边界；内部 MVP 已接通 Data、Judge 和 Quality，新版包格式已通过组件验证。下一项是实际产物打包、VERIFIED/READY 事务与完整 CLI 验收。完整出题闭环仍未完成。

## 目标与分流

先交付普通非 SPJ 题目的完整 CLI 闭环：

```text
Idea → Statement → Similarity
                    ├─ 查重通过 → Solution → 测试数据 → Docker/Judge → Quality → 打包 → READY
                    └─ 查重未通过 / 需要人工判断 → NEEDS_REVIEW
```

Quality 保留为现有确定性验收门禁：包不能绕过编译、样例、输入校验、差分和资源检查。首个闭环不增加自动 Idea 变异、自动内容重写或通用业务修复循环。

| 当前证据或结果 | 首个闭环的处理 |
|---|---|
| 当前题面和策略下的有效 ACCEPT | 提交并保留查重证据，进入 Solution |
| REJECT、人工复核区间、已取得但不足以接受的证据 | 保留结果并进入 NEEDS_REVIEW；不自动变异、不自动重新查重 |
| Idea 没有可行候选 | NEEDS_REVIEW，不自动生成下一批 |
| Solution/数据的内容验证失败或正确性存疑 | 保留失败证据，按既有错误类别进入复核或失败；不自动修改题意/代码 |
| 依赖不可用、网络故障、未知发送结果 | 沿用既有有界传输重试、BLOCKED/恢复和保守结算规则；不能当作查重通过 |
| 用户执行普通 Resume | 恢复当前阶段；NEEDS_REVIEW 不会因此变成已接受，也不会获得新的内容生成授权 |

至多一次 JSON 格式修复仍沿用已经验收的供应商协议，和业务变异分开。人工处理使用现有 ReviewDecision；首版不提供自动豁免相似性拒绝或质量门禁的捷径。

## 当前可复用基础

- 正式 Bootstrap/CLI 已具备 Idea/Statement/Similarity 预览、私有响应恢复、计量、取消和进程崩溃验收。
- `SimilarityReader` 可重建当前已提交的题面、查重输入、证据和决策；新主线直接使用这些证据，不读取变异配额。
- Slice 0 已有 Docker 编译/运行、watchdog、Judge 与包验证基础。按既有端口接入真实阶段，不重建执行框架。
- 原预览 revision 固定止于复核。通过新的显式编译 revision 接入后续阶段，保留旧 run 的原语义。

## 当前执行顺序

| 优先级 / 待办 | 直接交付物 | 完成标准 |
|---|---|---|
| P0 / LOOP-01：查重通过接 Solution | 当前有效 ACCEPT 才能进入 Solution；其他业务查重结果进入人工复核。复用已有证据提交边界和阶段事务，不引入变异授权层。 | ACCEPT 进入下一步；REJECT/复核/证据不足均无 Solution HTTP、无 mutation claim；暂停和重启不重复查重。 |
| P0 / SOL-01：可执行标准解 | 从已提交 ProblemSpec 生成 Reference、Brute 和解题说明；源码按现有产物协议保存。接入 Docker 编译并核对题面样例。 | 正确样例通过；编译错误/样例不符留证并停下；恢复不重复已提交模型调用；模型不能自报“验证通过”。 |
| P0 / DATA-01：可重现测试数据 | 生成 TestPlan、generator、validator，使用固定 seed；小数据用于对拍，正式数据覆盖样例、边界和规模。模型产生的程序统一在 Docker 内执行。 | 相同输入和 seed 可重现；输入均通过 validator；拒绝越界/超量数据；标准解生成正式答案。 |
| P0 / JUDGE-01：端到端验证 | 复用 Docker/Judge，完成 Reference 与 Brute 小数据差分、正式测试运行、checker 和资源门禁。 | 错解、非法输入、超时和资源超限均阻止出包；保存可定位的失败用例及真实执行证据。 |
| P0 / PKG-01：交付可用题包 | 汇总题面、标程、数据、validator/checker 及允许公开的说明；运行 Quality/PackageGate，导出题包。 | 同一 run 的验证结果、质量报告与 VERIFIED 包原子绑定后才能 READY；从导出包可重新运行验证。 |

Docker 的首次实际接线随 SOL-01 编译/样例验证开始，DATA-01 复用；JUDGE-01 完成整体数据联动。开发顺序始终沿一条可运行的正向链路推进。

## 当前进展：普通题闭环已通过实际验收

完整 MVP revision 已公开于 `config/mvp.example.yaml`，固定执行 Idea → Statement → Similarity → Solution → Data → Judge → Quality → Package。只有当前已提交的查重 ACCEPT 可以继续；非接受业务结果进入人工复核，普通 Resume 不产生自动豁免或变异授权。旧 preview 和 Solution selector 保持原阶段边界。

Solution 在真实 Docker 中编译 Reference/Brute 并核对样例；Data 编译 generator/validator，以固定 seed/序号/类型生成并重复核对原始输入；Judge 生成正式答案并完成 small 对拍；Quality 重建上游证明，执行固定 checker 的 AC/WA canary 和全部用例。Package 从当前证明组装规范 v2 ZIP，由专用 SQLite 事务原子提交 VERIFIED 包、质量绑定和 READY。

内部正向及 Data/Judge/Quality/package 提交中断恢复通过（226.146s）。独立 CLI 验收通过（208.994s）：真实子进程在包事务内退出，确认 occurrence、VERIFIED 记录和 READY 均未部分提交；未修改 CLI 恢复原 attempt 并导出 ZIP，拒绝覆盖。独立存储与执行身份仅从导出 ZIP 重编译 Reference/Brute，运行全部正式用例及 small 对拍。生成流程保持 4 次本地模型请求、1 次本地查重请求和 96 次容器创建；导出复验使用独立预算。

最新真实 Docker 失败路径回归也通过（327.428s）：样例错答、非法生成输入、不可复现数据、差分错答及标程超时均阻止出包。完整常规测试、vet、Linux 编译和 26 项架构检查通过；最终全量 race 已通过（SQLite 706.875s）。

验收范围为普通 C++ 题、本地 HTTP/TLS 供应商 fixture 和真实 Docker，未调用付费模型或外部查重服务，也不宣称外部服务可用性。证据及历史故障见 [完整包提交验收](../../evidence/mvp-package-commit-foundation.md)。配置及导出用法见 [配置说明](../../../config/README.md)。变异、SPJ、通用不可信包导入执行和 Go 实际闭环验收留在后续计划。

## 闭环验收

- 一条正向端到端场景：给定请求 → 查重通过 → 可编译标程和暴力解 → 合法测试数据 → 差分/正式 Judge 通过 → 题包导出与复验。
- 三类停下场景：查重未通过、生成内容/正确性失败、依赖不可用；结果可解释，复核和故障不会触发隐式变异。
- 在模型完成、产物发布、阶段提交和打包边界杀死真实子进程；恢复保持原调用次数、seed、预算和包绑定。
- 每个可执行切片进行对应定向测试和完整本地门禁；真实 Docker、供应商及查重服务验收分别记录，fixture 不能冒充外部服务验证。

## 暂停与以后重设计

MUT 系列后续、BR-01b、BR-02–BR-05、变异历史来源读取和原子变异授权全部退出当前关键路径。已有代码、迁移及通过的证据作为研究成果保留，尚未接入生产的部分不继续装配；只有闭环的具体缺口需要时才复用其中独立通用能力。

完整闭环可用后，再基于实际失败案例决定是否需要变异、如何简化输入和预算，以及采用何种最小恢复协议。旧 [变异路由计划](2026-09-09-slice2-business-routing.md) 仅作历史设计参考，不作为首个 MVP 的验收前置条件。SPJ、对抗增强和自动业务修复也不阻塞首条普通题闭环。

### 2026-09-10：真实模型补充验收

用户选择本地查重 fixture 后，APINode 的 `gpt-5.6-luna` 通过真实模型生成、Docker、READY、独立 CLI 导出及 ZIP 编译执行复验（371.970s）。本轮有 9 个用例、4 次模型调用、1 次查重 fixture 请求和 131 次生成流程容器创建。已修复并提交 SDK 对 GPT-5 模型采样参数的省略兼容问题；实际接口使用 `/v1`。这补充了上面的本地模型 fixture 基线，仍不代表真实查重或原创性通过。详见 [真实模型验收](../../evidence/apinode-live-mvp-2026-09-10.md)。