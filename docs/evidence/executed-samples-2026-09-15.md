# 执行生成样例验收（2026-09-15）

本次修改从 `e765e3b47d77208d9ab66e18e8f4d5c446397c62` 开始，在隔离 worktree 的 `codex/executed-sample-answers` 分支实现。设计与版本兼容边界见 [执行样例策略](../design/executed-samples.md)。

本文件是该旧分支的历史验收记录，仅保留作证据索引；它不能声称当前 V3 workflow 或迁移 000028 已完成同等验收。当前实现的验收必须使用当前代码、当前配置和新 run 的实际证据。

未调用付费模型；模型与 Similarity 使用本地 HTTP 响应夹具，程序编译/执行使用本机真实 Docker 与原固定锁 `D:/cpgen-private/toolchains/docker-v1.lock.json`。原失败 run、原验收文档及私有验收目录均未修改。

## 已验证行为

- 连通性回归：保留草稿答案 `1101000`，DSU 和独立全体顶点连通分量重标记均输出 `1100100`。两次编译、两次样例运行、12 个真实容器创建；读回已提交报告及重放不重复派发/计费。
- brute 能力边界：对 `n=201,m=0`，Reference 可执行，限制 `n<=200` 的 brute 非零退出；报告为 `sample.1.BRUTE.RE`，不通过。
- 不一致：错误 Reference 与正确 brute 不能产生通过报告；差异以 `sample.1.differential.WA` 表示，不把 Reference 当作未经证明的真值。独立调用、源码、输入、stdout、receipt、工具链与 cleanup 均参与读取校验。
- 跨阶段漂移：Judge 中双方一致但不同于 Solution 阶段已达成一致的样例执行结果时仍拒绝。旧报告版本、复用 std 执行充当 brute、缺失独立小数据证据均有拒绝测试。
- 另一题型的全链路：BFS/Floyd-Warshall 最短路题的草稿答案和解释故意替换为 `STALE_MODEL_*`。正式题面和包中的输出来自执行；旧解释不残留，原草稿没有被覆盖。
- 中断恢复：在 `judge/final-statement.json` 声明前注入中断，此时 Judge 报告、dataset、候选答案已经写出但没有提交。恢复保留同一 attempt，最终经过 Quality/Package 原子进入 READY。整个 run 仍只有 4 次夹具模型请求、1 次夹具 Similarity 请求和 100 次容器创建；Package 中断恢复不重复计入物理制品字节。
- 导出绑定：正式解释重新计算，最终题面、samples JSON、Judge 摘要和样例 `.in/.ans` 精确绑定；替换解释、输入、输出、题面、Judge 或移除 finalization 均拒绝打包。
- 程序请求投影：草稿答案/解释不进入 Solution/Data 模型变量；完整输入摘要仍绑定原始内容。JSON 数字用 `UseNumber` 保留超过 `2^53` 的种子与 int64 约束。

## 验证命令

| 检查 | 最终结果 |
| --- | --- |
| `go test ./... -count=1` | PASS；application 56.984s、SQLite 39.862s、integration 8.148s |
| 连通性、brute 能力、分歧、收据恢复与最短路全链路真实 Docker | PASS；221.101s，其中全链路 154.53s |
| `TestMVPPublicCLIResumesDataDraftAndExportsVerifiedPackage` | PASS；197.64s，覆盖公开 CLI 恢复、原子 READY、离线导出及 ZIP 独立编译执行 |
| 更新差异原因后的真实 `wrong_answer` 回归 | PASS；14.60s，保留失败报告 `sample.1.differential.WA` |
| 最后定向 domain/application/packageprobe 回归 | PASS，含精确大整数投影和正式制品绑定 |
| `go vet ./...`、`go build ./cmd/...` | PASS |
| `scripts/check-slice1-architecture.ps1`、`git diff --check` | PASS |

工作日志保留在此 worktree 的 `.tmp/sample-finalization/`（git 忽略目录）；本记录给出无需依赖临时数据库的命令、结果与边界。

普通测试默认不启用 Docker 或真实供应商；定向真实 Docker 测试显式设置：

```powershell
$env:GOFLAGS='-buildvcs=false'
$env:CPGEN_RUN_DOCKER_CANARY='1'
$env:CPGEN_DOCKER_TOOLCHAIN_LOCK='D:/cpgen-private/toolchains/docker-v1.lock.json'
go test ./internal/application -run 'TestSolutionExecutorRealDockerVerificationAndReplay/(connectivity_regression|brute_capacity|wrong_answer|run_receipt_gap)$|TestDataRunServiceRequiresRealPassingSolutionAndPreservesDraft/pass$' -count=1 -v -timeout 12m
go test ./internal/application -run '^TestMVPPublicCLIResumesDataDraftAndExportsVerifiedPackage$|TestSolutionExecutorRealDockerVerificationAndReplay/wrong_answer$' -count=1 -v -timeout 12m
```

`-buildvcs=false` 仅用于本 worktree 的宿主 Go 可执行文件构建，规避 VCS stamping 的 `exit status 128`；不改变 Docker 内 std/brute、Validator、checker 的编译执行策略或任何 Judge/Package 门禁。

首轮全仓检查发现新增 M27 后测试仍期待 26 个迁移；首次公开 CLI 检查发现测试仍期待两份已删除的模型 `.out` 制品。已更新计数断言（27 个迁移、31 个 Solution 制品、100 个全链路容器），未修改历史迁移或放宽生产条件。

## 限制

正式解释是确定性的输出说明，不是逐步算法推演。源码不同、独立执行和有限小规模差分不能证明两段任意程序的语义独立性或全输入正确性；任意自然语言正文/editorial 也没有通用语义证明器。该实现保留现有题意、验证器、完整 Judge/Quality 门禁，不因模型手算错误而拒绝一个执行一致的样例。

旧工作流 v1 的恢复/导出需保留原兼容二进制；本版本不会悄悄迁移旧 run 或重新解释旧证据。本次不代表真实模型端到端验收、真实查重服务验收、Linux 主机或完整 race 验收。
