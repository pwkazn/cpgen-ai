# V2 CLI 全流程验收（2026-09-15）

结论：修复 Windows watchdog 目录权限问题后，第二轮从公开 CLI `generate` 入口完成全部 12 个阶段，返回 `READY`，普通 CLI 成功导出题包。导出后独立编译、运行、答案计算及校验器反例测试全部通过。

本次范围为**真实模型 + 真实 Docker + 本地 Similarity fixture 的 CLI 集成验收**。没有配置真实查重服务，因此不能据此宣称真实查重服务已验收，或完全未注入测试依赖的独立 `cpgen.exe generate` 已验收。

## 环境与入口

- 模型服务：`https://api.zhuomatech.cn/v1`，模型 `gpt-5.6-luna`。
- 用户提供的密钥仅通过进程环境变量 `CPGEN_LIVE_API_KEY` 传入；配置只保存环境变量名称。
- Windows / Go 1.26.5；Docker 静态检查 `HEALTHY`，API 固定 `1.55`。
- 工具链锁：`D:/cpgen-private/toolchains/docker-v1.lock.json`，使用本机已安装的固定镜像。
- 工作流：`mvp.idea.statement.similarity.solution.data.judge.package.v2`。
- 基线 HEAD：`e765e3b47d77208d9ab66e18e8f4d5c446397c62`；执行的是包含 V2 改动和本次权限修复的工作区。
- 测试入口源码：`.tmp/cli-v2-live-20260915/main.go`；它直接调用 `cli.Run(os.Args[1:], os.Stdout, os.Stderr)`，使用生产 Bootstrap、配置解析、存储、预算、工作流、沙箱和 watchdog。
- 唯一外部服务替代：测试进程启动本地 HTTPS Similarity fixture，仅将目标 `93.184.216.34:443` 重定向到该 fixture；TLS 保留系统根证书并加入临时 fixture 证书，不关闭证书校验。模型仍访问指定服务，未注入模型结果。
- `generate` 后的读取、`resume`、`export` 均使用从 `cmd/cpgen` 构建的普通 CLI，且移除模型和 Similarity 凭据。

## 第一轮发现的问题与修复

第一轮：`run_1f8372b8f4d3ad9e152f664dad5d0203`。

创意、题面、查重、解法生成完成后，首次 `solution_verify` 返回：

```text
Access is denied.
expected sandbox execution version must be positive
```

没有创建 SandboxExecution 或 Docker 容器。后续 `resume` 返回：

```text
sandbox requires receipt recovery before another create:
prepared sandbox call differs from exact resource scope
```

根因：数据盘目录归当前用户所有，但继承权限仅为 Modify。`applyOwnerOnlyACL` 同时请求写入所有者与 DACL；即使所有者完全不变，写入所有者仍要求 `WRITE_OWNER`，从而拒绝访问。

修复位于 `internal/adapter/sandbox/docker/control_windows.go`：读取当前所有者；已属于当前用户时仅设置受保护的 owner/SYSTEM DACL，只有所有者确实不同时才请求变更所有者。没有扩大允许访问的主体。

验证：

- 新增 `TestOwnerOnlyACLDoesNotRequireChangingExistingOwner`，构造 owner 已正确、仅有 Modify 权限的目录。
- 将旧版 `control_windows.go` 通过 Go overlay 代入时，该测试稳定报 `Access is denied`；修复后通过。
- `go test ./internal/adapter/sandbox/docker -count=1` 通过，包括既有活动 watchdog 管道保护测试。

第一轮消耗 3 次模型调用，之后通过公开 `run cancel --reason ...` 结束为 `CANCELLED`；所有调用已终止、预算预留为零，无容器创建。未删除调用记录、修改数据库、重置预算或人工替换模型内容。

已知限制：这种在沙箱执行记录建立之前失败、相关调用已结算为未发送终止的运行，现有 `resume` 不能直接恢复。本次修复目录权限原因，没有扩展其恢复语义；第一轮证据保留。

后续修复：[沙箱未发送恢复验收](sandbox-unsent-recovery-2026-09-15.md) 补充了这条 `solution_verify` 路径的恢复语义。上述限制描述的是本次原始验收时的行为；原第一轮已取消的运行仍保持 CANCELLED，未改写其调用或预算记录。

## 第二轮 CLI 结果

运行 ID：`run_1b8cbb63b4a8782abce969ff61cd8e55`。

私有证据目录：`D:/cpgen-private/zhuoma-cli-v2-20260915-02`。

题目：**Best Edge to Cut in a Tree**。初始请求为普通无向无权图题，要求独立暴力解；没有指定已有成功题目或人工提供解法。有效 seed 为 `202609150201`。

首轮原预算为 8 次模型调用。第二轮新请求的上限为剩余 5 次，最终使用 4 次；两轮合计 7 次，未超过本次验收声明的 8 次上限。JSON 格式修复上限为 1，V2 内容重生成全局上限为 2；本轮均未触发。Similarity fixture 预算为 6 次，以覆盖可能的题面重生成。

| CLI 命令 | 退出码 | 结果 |
| --- | ---: | --- |
| `version --json` | 0 | Go 1.26.5 |
| `config validate` | 0 | VALID |
| `config effective --redact` | 0 | VALID |
| `doctor --json ...` | 0 | HEALTHY |
| `generate --request request.json`（验收入口） | 0 | READY，约 310.8 秒 |
| `run show` | 0 | READY |
| `run list` | 0 | OK |
| `run events` | 0 | OK |
| `review show` | 0 | 无待审项 |
| `run resume` | 0 | READY，无新调用，版本仍为 349 |
| `run export --output problem.zip` | 0 | EXPORTED |

12 个阶段全部 `SUCCEEDED`，每个阶段 attempt_count 均为 1：

```text
idea → statement → similarity → similarity_decision
→ solution → solution_verify → solution_decision
→ data → data_verify → judge → quality → package → READY
```

`content_retries` 为 0，因此本次真实模型运行验证了 V2 正常通路，未实际触发内容失败后的自动重生成。先前固定模型输出的真实 Docker 重试测试不能替代这里的真实模型重试覆盖。

## 导出后的独立验证

- ZIP 的参考解和暴力解重新在真实 Docker 中编译，不复用主流程的二进制。
- 参考解通过包内全部 7 组测试，暴力解通过其中 3 组 small 测试。
- 独立 Python 实现对每条树边计算 `s*(n-s)` 并处理字典序决胜；7 组答案全部一致。
- 数据包含 2 组样例、3 组 small、1 组最小边界、1 组 stress；最大规模为 128,850 个顶点。受单个输入一 MiB 限制，本包没有覆盖声明的 200,000 顶点上限。
- 从 ZIP 重新编译 validator，12 组独立测试全部符合预期：3 组合法输入返回 0；9 组非法输入返回 3，覆盖 n 越界、自环、顶点越界、重复边、成环且不连通、缺边、多边、尾部垃圾。
- `TestCLIExportAcceptanceRevalidation`：41.15 秒通过。
- `TestLiveExportValidatorCases`：40.97 秒通过；组合测试总计 82.412 秒。

题包：`D:/cpgen-private/zhuoma-cli-v2-20260915-02/problem.zip`，1,078,352 字节。

```text
archive SHA-256:
fefa9b93c140d1c0c49dac07cc78c1dbba2ffebbe94ed86a996cf5c3a7e111f0

package_id:
sha256:633684910277b3326d001ba22c28a4fae518e956ce4557aa8c08e48369b8d118
```

## 计量与清理

第二轮结算：

| 指标 | 数值 |
| --- | ---: |
| 实际模型调用 | 4 |
| 输入 / 输出 token | 9,907 / 8,293 |
| 模型成本保守记账 | 400,000 micro-USD |
| Similarity fixture 调用 | 1 |
| Docker 容器创建 | 109 |
| SandboxExecution | 45，全部 CLEANED |
| 沙箱资源 | 168，全部 CLEANED |
| 持久调用记录 | 400，全部 TERMINAL |
| 物理调用 | 400 COMPLETED，10 ABORTED_NO_DISPATCH |
| 预算剩余预留 | 所有维度为 0 |

模型成本为配置上界的保守结算，不能当作服务商实际账单。未发送的物理调用包含未使用的预留尝试；没有 PREPARED、DISPATCHING 或 UNKNOWN 悬挂调用。上述沙箱计量仅为主流程，独立 ZIP 验证使用单独的测试账户。

额外直接查询 Docker 中带本轮 `org.cpgen.run` 标签的容器和卷，均为空。第一、第二轮私有输出的凭据模式扫描未发现密钥样式内容。

机器可读汇总：私有目录的 `acceptance.json`。详细记录包括 `commands.json`、`audit.json`、`generate.json`、`run-export.json`、`export-revalidation.json`、`independent-answer-check.json`、`validator-acceptance.json` 和 `export-tests.log`。验收辅助源码和原始失败证据均保留在对应私有目录；仓库未保存密钥。
