# CLI 最终验收重跑（2026-09-15）

本次真实模型验收未通过：新候选的第一组样例答案错误，被真实 Docker 样例验证拦截，未产生 READY 或题包。此前成功候选不替代本次结果。本轮只生成一个新候选，没有重抽、修改生成制品或豁免门禁。

## 环境与范围

- 基线提交：`e765e3b47d77208d9ab66e18e8f4d5c446397c62`。
- Windows amd64、Go 1.26.5、Docker Engine 29.7.2。
- 用户指定模型：`gpt-5.6-luna`；兼容 API 前缀：`https://api.zhuomatech.cn/v1`。
- 使用原固定工具链锁 `D:/cpgen-private/toolchains/docker-v1.lock.json`，没有重建镜像。
- 沿用此前成功候选的完整手动请求：在线连通性接受位，n ≤ 50000、m ≤ 75000，独立暴力解、普通 C++、唯一输出；每次最多 8 次模型调用。本次不是自由题意生成成功率实验。
- Similarity 是已有的本地 TLS fixture；真实查重服务未配置，不能据此证明原创性。

真实生成通过 `TestLiveProviderMVPWithFixtureSimilarity` 的应用服务入口运行；CLI 单独执行配置校验、状态/事件/审核读取、恢复和导出。不能将此记录描述为独立生产 CLI 从 `generate` 到 READY 全程通过。公开 CLI 工程测试另用确定性供应商夹具，验证真实 Docker、进程中断恢复、导出和 ZIP 独立编译执行。

## 本次真实生成

验收测试耗时 126.10s，返回 FAIL。运行 ID：`run_ca074cfc318cd51f78b005a09eff8451`。题目为 **Online Connectivity Decisions**。

第一组样例输入：

```text
5 7
1 2
2 3
1 3
4 4
3 4
2 4
5 5
```

题面给出 `1101000`，正确答案为 `1100100`：接受第 1、2、5 条边，自环和已连通端点间的边拒绝。Reference 实际输出正确答案；独立维护连通分量集合复核也得到相同结果。其余两组样例独立核验通过。题面解释称接受第 1、2、5 条边，与其自身样例答案矛盾。

Reference/Brute 均编译成功，验证报告原因 `sample.1.SOLUTION.WA`，摘要 `sha256:a707ec35994d3f32cc62cf1da5eac4620c38ea26808c511de73f1e18727075af`。最终状态 `NEEDS_REVIEW/solution_decision`，版本 150；Data、Judge、Quality、Package 保持未执行。

预算记录：3 次真实模型请求，6616 输入 token、2886 输出 token；1 次 Similarity fixture 请求，10 次运行内 Docker 容器创建。模型保守成本扣减 300000 micro-USD（0.30 USD），不是供应商实际账单。全部调用记录终结，预算预留均已结算或释放，VERIFIED 包数量为 0。

## 独立 CLI 检查

| 命令 | 退出码 | 结果 |
| --- | ---: | --- |
| `config validate` | 0 | VALID |
| `run show` | 0 | NEEDS_REVIEW，版本 150 |
| `run list` / `run events` / `review show` | 0 | 返回合法 JSON |
| `run resume` | 6 | 保持 NEEDS_REVIEW、版本 150，没有重复生成 |
| `run export` | 9 | `package export requires READY`，没有生成 ZIP |

以上退出码以当前实际 CLI 为准。历史 `docs/design/cli.md` 的命令及退出码表与实现存在差异，不用该旧表解释本次结果。

## 工程检查

- 全量无缓存普通测试：PASS，application 48.158s、SQLite 31.404s、integration 6.823s。
- `go vet ./...`、`go build ./cmd/...`、架构检查：PASS。
- `go test -race ./internal/cli -count=1 -timeout 5m`：PASS，27.991s。
- `TestMVPPublicCLIResumesDataDraftAndExportsVerifiedPackage`：真实 Docker PASS，190.56s。覆盖 CLI 零预算生成分流、数据草稿恢复、Package 事务中断、原子 READY、离线导出、拒绝覆盖以及 ZIP 独立编译/执行。该测试的模型与查重响应均为夹具，不能替代上面的真实模型失败。
- `git diff --check`：PASS。
- 本次未重跑完整全仓 race、Linux 执行或历史 A/B Docker 探针。

当前源码缺少此前补充验收文档记载的 `CPGEN_LIVE_REQUEST` 入口。本次补齐了测试中的可选绝对路径、严格 JSON 解码和完整请求替换，并在配置说明中记录用法；默认仍跳过付费模型测试。没有修改生产门禁或模型提示词。

## 证据保留

私有目录：`D:/cpgen-private/zhuoma-cli-final-20260915-01`，含请求、配置、模型响应、SQLite、Docker 制品、`solution-verification.json`、`independent-samples.json`、`cli-acceptance.json`、`final-audit.json`、预算及事件。日志在 `.tmp/cli-final-20260915/`。

密钥仅用于验收进程环境，没有写入配置或报告；新证据及日志中的密钥模式扫描无命中。真实模型运行的 15 条沙箱资源记录全部为 CLEANED。所有本轮启动的验收进程已结束。

开始时已有的三份验收记录和校验器补验源码保持原状；本次修改未提交或推送。
