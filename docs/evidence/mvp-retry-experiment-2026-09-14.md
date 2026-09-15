# MVP 单次重生成实验（2026-09-14）

结果：**重生成一次仍未通过样例验证**。用户要求临时修改后再试一次；本次只增加一个候选，保留旧候选及其失败记录，没有接入正式自动重试功能。临时源码修改已撤回。

## 请求、预算与执行

沿用 `https://api.zhuomatech.cn/v1`、服务列出的 `gpt-5.6-luna` 和真实 Docker；Similarity 仍为本地 TLS fixture。

首轮为 `run_bd7bedfda65137dd7cc6e88044976bdb`，详见 [最终验收记录](mvp-final-acceptance-2026-09-14.md)。新候选为 `run_71d4c7f8afa17a8fa8fb9995f75fc25a`，使用相同 brief，将 seed 从 `202609101831` 改为 `202609101832`。从旧运行的 SQLite 只读核对请求摘要、失败报告、已消耗预算及无 VERIFIED 包后，给新候选分配剩余额度，没有重置总调用预算。

| 项目 | 首轮消耗 | 本次新候选消耗 | 两轮合计 |
|---|---:|---:|---:|
| 模型调用 | 3 | 3 | 6 / 原上限 8 |
| 输入 token | 5285 | 5363 | 10648 |
| 输出 token | 5053 | 4534 | 9587 |
| 模型保守成本记账（micro-USD） | 300000 | 300000 | 600000 / 原上限 800000 |
| Similarity fixture 请求 | 1 | 1 | 2 / 原上限 2 |
| Docker 容器创建 | 10 | 14 | 24 / 原上限 256 |

保守成本记账不是供应商账单。编译/样例执行经过真实 Docker；未执行生成程序于宿主机。

## 内容失败

新题为 **Maximum-Value Connected Subtree**：在树中选择恰好 k 个顶点，要求诱导子图连通，使权值总和最大。

Reference 和 Brute 均编译成功，第一组样例两者均通过。第二组样例：

```text
4 2
-5 7 -2 6
1 2
2 3
3 4
```

模型给定答案为 `4`，并声称应选顶点 3、4；Reference 输出 `5`。恰好选择两个连通顶点等价于选择一条边，三种合法选择的权值和分别为 `2`、`5`、`4`，所以独立核对的正确值确实是 `5`。

运行停在不可豁免的 `NEEDS_REVIEW/solution_decision`，原因 `sample.2.SOLUTION.WA`。第二组样例在 Reference 失败后立即停止，未声称 Brute 已执行这一组。Data、Judge、Quality、Package 不继续，没有 VERIFIED 包或 ZIP。未生成第三个候选，也未直接改写模型给定答案。

验证报告摘要：`sha256:d18ef0e6571c8866e123871961ffb52779c6b1a4f31651fda0a63b8b9f4731ae`。

## 临时实现和实验过程中的问题

临时给 `TestLiveProviderMVPWithFixtureSimilarity` 增加指定请求与恢复入口，以剩余预算的新请求生成一个候选。实验中还暴露了两个预算/恢复边界：

1. 首次新候选完成 Idea、Statement 后，因只剩 1 次 Similarity 额度，而适配器计划一次预留 2 次，停在 Similarity，尚未发出查重请求。此停止属于实验预算设置问题，不是内容验证结果。
2. 直接将 Similarity 的重试策略上限从 2 改为 1，会与题面提交时冻结的下游输入摘要不一致，恢复被拒绝；没有新增模型调用。随后保持原冻结策略，仅临时将实际物理调用计划缩减为一次。协调器仍校验实际调用数不超过原重试上限，并通过正常预算预留、授权、结算和制品提交。

使用现有 `CreateReview(RETRY)` / `Resume` 接口，记录“减少实际预留次数、预算不增加”的条件证据，继续同一个候选；没有直接写数据库、豁免内容门禁或重新生成 Idea/Statement。修正预留后，查重发出一次，解答生成和真实样例验证继续，最终得到上面的内容失败。

临时修改涉及 `internal/application/live_provider_test.go` 和 `internal/execution/similarity_calls.go`，已完整逆向撤回，`git diff --exit-code --` 两个文件通过。撤回后的定向 Similarity 调用回归和验收入口编译通过。前一轮验收已有的文件修改保持不变。

这次实验验证了一个新候选的完整前半段，但没有证明自动重试能稳定产出合格题包。正式实现还应处理“剩余额度小于预留的最大尝试次数”，并在冻结输入策略不变的条件下缩减实际尝试计划。

## 留存证据

私有目录：`D:/cpgen-private/zhuoma-live-mvp-20260914-02`，保留 `retry-source.json`、`retry-request.json`、`retry-condition.json`、`resume-review.json`、`result.json`、`budget.json`、`events.json`、`failure-analysis.json`、SQLite 和原始制品。

预算预留停止时的快照保留为 `reservation-stop-result.json`、`reservation-stop-budget.json`、`reservation-stop-events.json`。实验日志位于 `.tmp/mvp-final-20260914/live-retry*.log`；可复核的最终临时补丁位于 `.tmp/mvp-final-20260914/temporary-retry-final.patch`。这些私有生成内容、密钥和实验二进制未提交到源码仓库。
