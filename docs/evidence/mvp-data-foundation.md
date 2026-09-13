# MVP 测试数据执行证据

日期：2026-09-09。对应 [DATA-01](../superpowers/plans/2026-09-09-mvp-generation-loop.md)。内部 MVP 流程已完成 Data 草稿生成、真实 Docker 编译、两次独立复现、输入校验和数据清单提交，并接入 [Judge](mvp-judge-foundation.md) 与 [Quality](mvp-quality-package-foundation.md)。完整 MVP 配置尚未公开，实际打包/导出/READY 仍需完成。

## 已实现

- `DataExecutor.ReadInput` 重建当前 ACCEPT、题面、Solution 内容与已提交 Docker 样例报告。Solution 失败直接复核，没有 Data 模型调用。
- 模型只提交 generator、validator 和 4–12 项用例设计，至少两个 small、一个 boundary、一个 stress。本地绑定上游摘要、语言和原始 seed，按用例序号派生 64 位 seed；模型不能提交 argv、路径、资源覆盖或通过结论。
- 内置草稿 prompt/schema、一次 JSON 格式修复、私有输出恢复、计量和缓存继续复用现有协议。已提交草稿可按原源码、用例顺序和 seed 重建。
- `DataVerifier` 在固定工具链中编译 generator/validator，先校验题面样例，再逐例运行生成器两次并比较原始 stdout 字节。复现使用两个稳定、不同的执行范围；要求逻辑调用和物理调用不重合，不能把缓存重放当成独立执行。
- 复现一致的输入经 validator 验证后作为正式输入提交。CE、非法输入、非确定性、TLE/MLE/OLE 等保留首个失败报告；失败不产生数据清单，也不进入 Judge 执行。
- `ReadVerification` 只读重建源码、计划、精确 Docker 请求、结果收据、输出和 CLEANED 记录，验证数据清单内容并拒绝缺失或额外产物。读取不会执行 Docker 或重新调用模型。
- 内部阶段顺序为 `idea → statement → similarity → similarity_decision → solution → solution_verify → solution_decision → data → data_verify → judge → quality → package`。旧预览和公开 Solution revision 的边界保持原语义。

## 固定执行协议

生成器接收三个参数，顺序固定：`--seed=<uint64 decimal>`、`--case=<one-based ordinal>`、`--kind=small|boundary|stress`，向 stdout 输出一份完整输入。Sandbox 端口只接受这些有类型、有限范围的参数；构造执行目标时复制 seed 和参数，调用者之后修改指针不能改变实际 argv。

validator 从 stdin 读取输入；合法退出 0，非法退出 3，stdout 必须为空。生成器/validator 使用题目指定语言和标准库，固定每程序 2 秒、256 MiB、64 PIDs；每例生成输出至多 1 MiB，validator stdout 至多 4096 字节。模型被要求完整校验格式和约束，包括多余非空白输入；该要求仍依赖生成的 validator 实现，执行通过本身不证明其语义完备。

`data/verification.json` 绑定草稿、工具链、策略和逐例执行证据。全部通过才提交 `data/dataset.json`，列出样例与生成输入的路径、来源、序号、种类、seed 和 Blob 身份。清单只包含已验证输入；答案和差分结论由 Judge 单独产生。

## 验证与范围

- Domain 测试覆盖越权字段、重复/缺失字段、过量计划、必要用例缺失、源码上限、seed/序号及上游摘要替换。Sandbox 参数测试拒绝越界序号、非法种类和角色，并确认 seed/参数快照及完整请求的执行身份不同。
- 组件测试覆盖通过、CE、非法样例、非法生成输入、非确定性、生成器 TLE 和共享物理执行。报告缺少编译、样例、复现运行或 validator，以及替换 seed，均不能通过。
- `TestDataRunServiceRequiresRealPassingSolutionAndPreservesDraft` 使用本地模型/Similarity HTTP fixture、真实 SQLite、Docker 和 detached watchdog。Data 通过时产生 6 份合法输入；进入 Judge 前共使用 52 次容器创建、4 次模型调用和 1 次查重。上游样例 WA 只有 3 次模型调用；非法生成输入和非确定性分别使用 34、32 次容器创建并停在 Judge 入口复核。
- Data 报告发布前故障注入后，新的服务实例复用原 attempt、已完成执行和产物；不增加模型调用或容器创建。删除已提交计划、清单、输入或源码后，只读证据重建失败。
- Data 四路径、组件测试、独立公开 Solution CLI 和取消恢复的合并竞态测试通过（application 455.648s）。加入 Judge 前的完整普通测试通过（application 42.006s；SQLite 29.666s）。后续 Judge 的验证结果见其证据文档。

没有调用付费模型或外部 Similarity 服务；上述程序编译、执行和清理是真实 Docker。故障注入覆盖 Data 报告发布间隙，不等同于在 Data 阶段杀死真实进程。变异机制继续退出关键路径。
