# 真实模型生成链路补充验收（2026-09-15）

用户要求继续跑通生成链路。本记录接续 [最终验收](mvp-final-acceptance-2026-09-14.md) 和 [单次重生成实验](mvp-retry-experiment-2026-09-14.md)，保留每次失败，不将后续成功解释为无条件成功率或完整外部服务验收。

结果：**真实模型 → Docker 验证 → READY → CLI 导出 → ZIP 独立执行，以及额外校验器补验，均已通过**。成功候选为 `run_3f42b0aad2397fb100e081850cf96da3`，题目 **Online Connectivity Decisions**。

真实模型使用 `https://api.zhuomatech.cn/v1`、`gpt-5.6-luna`，编译与执行使用固定工具链的真实 Docker。Similarity 仍为本地 TLS fixture，不能证明原创性，也不代表历史 A/B 探针或完整真实查重验收已通过。每个候选使用独立目录和明确的 8 次模型调用上限；各轮消耗分别记账。

## 成功候选的完成证据

- 主验收 `TestLiveProviderMVPWithFixtureSimilarity`：PASS，435.813s。SQLite 中当前运行为 `READY/package`，且存在唯一对应的 `VERIFIED` 记录；所有调用终结，预算预留归零。
- 三组样例均通过 Reference/Brute，另以显式连通分量集合独立复核。五组生成数据包含三个 small、一个 boundary、一个 stress；生成器复现、校验器、Judge、Quality 全部通过。
- 包中共有 8 组输入/答案、29 个文件。最大压力输入确实为 `50000 75000`，866740 字节，答案 75001 字节；未以较小数据替代已声明的最大规模。
- 独立 CLI 在移除两类供应商凭据的进程环境中导出。导出字节与已提交归档一致；之后只从 ZIP 重新编译程序，对全部测试运行 Reference，对 small 数据运行 Brute，并核对答案。
- `TestLiveExportValidatorCases` 使用 `-race`、真实 Docker，只从同一个 ZIP 编译校验器：PASS，69.334s。12 组输入覆盖合法空图/自环/最大顶点数、两个曾被误放行的越界顶点、顶点数上界、完整的超边数输入、零/负顶点、整数溢出与尾随数据；合法退出 0、非法退出 3，全部符合预期。
- 对归档逐项核对测试输入/答案的字节数和 SHA-256，并核对 SQLite VERIFIED 记录、CLI 导出和补验结果的包身份一致。

归档位于 `D:/cpgen-private/zhuoma-live-mvp-20260915-07/problem.zip`（1347749 字节）。包 ID 为 `sha256:8b1cb6435a0f45ed3e26920130aaac2c528c14b396ef321b6686712237a4caa5`，ZIP 摘要为 `sha256:a62d271506e383f5ce15baf8b0e9e863b449dd1120500a5d0d7764e0850e1517`。

成功候选消耗 4 次模型调用（10765 输入 / 5792 输出 token）、1 次 Similarity fixture 请求、113 次运行账本内容器创建。此补充验收的五个候选合计 19 次模型调用、48628 输入 / 34624 输出 token、5 次 fixture 请求。保守模型成本记账为 1900000 micro-USD，**不是供应商账单**；独立 ZIP/校验器执行使用各自单独的沙箱测试预算。

## 已确认的失败与改进

前两次生成分别存在错误样例答案（7 应为 6、4 应为 5）。本轮保留普通无向无权图、C++、medium 难度和独立暴力解的请求范围，增加样例独立核算要求；未手改任何已生成题面、程序或期望输出。

| 候选 | 结果 | 模型调用 | 运行账本内 Docker 容器创建 |
|---|---|---:|---:|
| `run_d22a49f04815a4de55442d9fca2f5b04` | 样例通过；生成器错误比较 `--kind=small` 和 `small`，退出 2，停在 `NEEDS_REVIEW/judge` | 4 | 30 |
| `run_7b2a0d8210edead2f721ddb46bcca9f1` | 样例、数据验证通过；正式压力数据答案超过 1 MiB，停在 `NEEDS_REVIEW/quality` | 4 | 86 |
| `run_76d13e6adc5a8b3ce9822d40e88b476e` | 主链路 READY、ZIP 独立执行通过；额外非法输入补验发现校验器放行越界顶点，不能视为完整合格 | 4 | 120 |
| `run_e5e5a377d514bfd5534ded1b6005d7ab` | 新题面第二组样例含环且分量数错误，停在 `NEEDS_REVIEW/solution_decision` | 3 | 14 |
| `run_3f42b0aad2397fb100e081850cf96da3` | 主链路、独立 ZIP 执行、12 组校验器补验均通过 | 4 | 113 |

表中容器计数来自各运行的预算账本，不含独立 ZIP 重编译/执行及校验器补验的单独测试账本。

第一项题目为 **Lexicographically Smallest Bipartite Coloring**。独立穷举两组样例分别检查 32、8 个染色方案，题面答案均正确。数据验证报告为 `sha256:344cb8ecaf4ec86a48877c42bb6a2d36c0eb90a321d61219ed0678e02177591a`，原因 `generated.1.run.1.RE`。源代码审查另发现校验器用无符号 `limit - digit` 时可能下溢，后续请求同时说明参数前缀与数字上界要求。

第二项题目为 **Online Spanning Forest**。独立维护连通分量集合核对样例；新生成器和校验器已修正上述问题，真实 Data 验证通过。第六组生成数据有 200000 条边，每条决策占 9 字节，仅这部分答案就需 1800000 字节，超过 Judge 固定的 1048576 字节 stdout 上限。报告为 `sha256:5ba9da2dc2ac7610491feb6394bfda863942f033944a1bc3a9f7d7187435eb7c`，原因 `generated/006.in:reference.OLE`。

第三项题目为 **Online Maximum Forest**，补充实际 I/O 上限后选择 n ≤ 50000、m ≤ 80000，标准格式输入上界 960012 字节、输出上界 80001 字节。满规模数据、主链路 READY、CLI 导出和 ZIP 独立编译执行通过（535.465s）。但源码审查发现校验器仍使用可能下溢的 `limit - digit`。额外从 ZIP 编译校验器并实测：`1 1\n2 1\n` 和 `1 1\n1 9\n` 均错误退出 0，预期应为 3；其余四组正负输入符合预期，补验总体失败（24.834s）。原 `acceptance.json` 只表示主链路通过，新增 `validator-acceptance.json` 明确记录补验失败，不能以 READY 掩盖已知缺陷。

后续请求继续要求满规模输入/答案满足实际字节上限，并给出安全的数字比较方式、显式最终区间检查和上述两个反例。未扩大 Judge 输出限制、删去压力用例、豁免验证或直接编辑 SQLite/生成制品。

第四项再次出现样例内容错误：顶点 5 只有自环，输入图实际有两个分量，最大森林只能有 3 条边；题面却给出 4 条边及 1 个分量，解释自身也承认示例无效。报告为 `sha256:1e03e382fca140ca1bf722f4b5a226f3d547ca1d256f8436cd4eaddc60631fb2`，原因 `sample.2.SOLUTION.WA`。随后停止堆叠历史说明，改用简短明确的手动请求，固定此前已通过主链路的唯一输出任务：依到达顺序接受连接不同分量的边、逐边输出接受位。保留独立暴力解、普通图题与满规模执行，约束明确为 n ≤ 50000、m ≤ 75000。此调整不能证明原始自由题意生成的成功率。

## 验收入口与回归

`TestLiveProviderMVPWithFixtureSimilarity` 新增可选 `CPGEN_LIVE_REQUEST` 绝对路径入口，严格解码完整 `RunRequest`，不从默认请求补齐缺失字段，并将最终请求记录为 `request.json`。新目录的一次性 `started` 标记、显式付费调用开关、预算、固定工具链、CLI 导出和独立 ZIP 执行要求保持不变。这是验收入口，不是生产自动内容修复或重试功能。

新增入口的默认跳过与配置/Similarity 定向回归通过；增加校验器补验后的全量 `go test ./... -count=1 -timeout 15m` 再次通过（application 55.591s，SQLite 38.812s），`go vet ./...`、`go build ./cmd/...`、架构检查通过。新补验路径已通过真实 Docker 的定向 race；此前完整 race 验收证据保留，本轮未新增生产执行路径。

新增显式 `TestLiveExportValidatorCases`，读取 `CPGEN_LIVE_ROOT/problem.zip` 和绝对路径 `CPGEN_LIVE_VALIDATOR_CASES`（1–32 个 `{name,input,exit_code}` 对象，退出码限 0/3）。它仅在真实 Docker 沙箱中重新编译 ZIP 内校验器，以独立预算和调用账本执行人工给定反例，保留 `validator-acceptance.json`；不调用模型或修改原题包。默认测试跳过此项。此补验按当前题意选择输入，不能把特定图题的反例套用于任意题目。

## 私有证据

- `D:/cpgen-private/zhuoma-live-mvp-20260914-03`：请求、预算、事件、失败分析、独立样例复核、SQLite 与制品。
- `D:/cpgen-private/zhuoma-live-mvp-20260914-04`：同上，包含通过的 Data 报告和失败的 Judge 报告。
- `D:/cpgen-private/zhuoma-live-mvp-20260915-05`：主链路通过的 ZIP、独立样例/字节上界核算，以及失败的校验器补验记录。
- `D:/cpgen-private/zhuoma-live-mvp-20260915-06`：补齐显式数字后置检查的新候选目录。
- `D:/cpgen-private/zhuoma-live-mvp-20260915-07`：精简唯一输出请求、三组独立样例核算和十二组校验器补验输入。
- `.tmp/mvp-final-20260914/live-checked-samples.log`、`live-interface-diagnostics.log`、`live-io-contract.log`：各轮独立日志。

密钥只进入进程环境，没有写入配置、报告或版本控制。以前失败的运行保持原状态。

成功目录另含 `acceptance.json`、`validator-acceptance.json`、`final-audit.json`、`package-record.json`、`export.json`、完整预算与事件。最终日志为 `.tmp/mvp-final-20260914/live-concise-contract.log` 和 `live-validator-negative-fixed.log`。所有本轮验收进程均已结束，修改留在工作区，未提交或推送。
