# MVP Judge 接线证据

日期：2026-09-09。对应 [JUDGE-01](../superpowers/plans/2026-09-09-mvp-generation-loop.md)。内部流程已接入正式答案生成、小数据差分、资源结果判定及 [Quality](mvp-quality-package-foundation.md)。实际打包、导出与 READY 绑定尚未完成，公开 Solution revision 仍止于原检查点。

## 执行与证据

- `ReadJudgeInput` 只接受当前已提交且通过的 Solution 和 Data，重新验证其源码、输入、执行收据及清理记录。Judge 复用已保存的 Reference/Brute 编译产物，不调用模型，也不重新编译它们。
- 每份合法输入都在 Docker 内运行 Reference，使用题面的时间/内存限制、64 PIDs 和 1 MiB stdout/stderr 上限。样例输出再次按 `exact-tokens-v1` 核对；每份 generated/small 输入另运行 Brute 并比较实际 stdout 的 token 摘要。
- Reference 与 Brute 必须来自不同的逻辑和物理执行。每个通过用例保存 Reference 的原始输出作为答案；样例不符、差分不符、RE/TLE/MLE/OLE 保留首个失败用例并停止。基础设施错误不能伪装成内容通过或内容失败。
- `judge/verification.json` 绑定当前 Data 报告、Solution 报告、工具链和检查策略。通过才发布包含全部输入/答案及 Judge 摘要的 `judge/dataset.json`；失败报告不能产生这份清单。
- `ReadJudgeVerification` 重新派生每次完整请求，核对当前已提交收据、来源调用、CLEANED 资源、保留 stdout、token 比较和答案字节，并拒绝缺失或额外产物。只读路径没有执行或写入能力。
- Data 失败在 `judge` 入口复核；Judge 失败在 `quality` 入口复核。通过后现在继续执行 Quality，并在尚未实现的 package 入口复核。普通 Resume 不会越过人工复核或重新生成代码。

## 本地验证

- 首条真实 Docker 通过路径已完成 6 份输入/答案和 2 次小数据差分，累计 68 次容器创建、4 次本地模型 fixture 调用、1 次本地查重 fixture 调用（101.721s）。
- 六路径完整 Docker 验证通过（411.181s）：通过、上游样例 WA、非法生成输入、非确定性、生成数据差分 WA、Reference TLE。后两种程序通过全部题面样例，在 `generated/002.in` 和 `generated/003.in` 才失败，分别消耗 64、66 次容器创建，保留准确失败原因且没有完整答案清单。
- 通过路径分别在 Data 和 Judge 报告声明前注入中断；每次由新的服务实例 Resume，保留原 attempt ID/ordinal 和 68 次容器计费，没有增加模型/查重调用。只读验收拒绝缺失 Judge 清单、样例答案或生成答案的已提交阶段。这里是报告发布故障注入，不等同于实际杀死 Judge 进程。
- 组件测试覆盖通过、样例 WA、差分 WA、Reference TLE/OLE、Brute MLE 和共享执行身份；报告缺少答案、用例或 Brute，或替换 seed、比较摘要及策略，均被拒绝。组件竞态测试通过（23.395s，含生成器请求身份检查）。
- 完整普通测试通过（application 42.810s；integration 4.870s），vet 和 26 文件架构检查通过。
- 固定 C++ checker 已作为可导出的源码实现，宿主端共用 `judge.ExactTokenDigest`，保持原报告摘要字节不变。真实 Docker 编译与 10 组比较/边界用例通过竞态测试（64.959s），覆盖全部 Go `unicode.IsSpace` 字符、非空白 Unicode、非法/截断 UTF-8、NUL、数字文本差异、缺失/额外 token 和 1 MiB 文件上限；重放不新增资源计费，产物原子提交通过。
- 正向恢复、生成数据差分 WA 和 Reference TLE 的真实 Docker 定向竞态验证最终通过（719.146s）。引入固定 checker 后再次完整普通测试通过（application 42.958s；integration 4.873s），vet、Linux/amd64 CLI 编译、26 文件架构检查、380 个 Go 文件的 gofmt 和 diff 检查通过。

以上供应商响应均为本地 fixture；真实执行使用现有开发 Docker lock。普通题采用宿主端固定 token 比较，C++ checker 已独立验证并接入当前 Quality。下一步将其绑定最终导出包，完成 PackageGate 与原子 READY 事务；结构包检查本身不足以证明题目已经过验证。
