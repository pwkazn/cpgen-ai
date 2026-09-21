# CPGen — 可审计的竞赛编程题目生成

CPGen 把结构化请求转化为**经过验证、可复现的题包**：题面、参考题解/暴力题解、
generator、validator、checker、测试数据，以及规范的 ZIP 导出。

设计前提是：语言模型只是*候选生成器*，绝不是权威来源。每一项验收决定都由确定性
代码做出——编译器、沙箱中的真实执行、差分评测、查重策略，以及题包门禁。模型即便
编造出错误的参考题解，产生的也只是一个**被拒绝的 run**，而不是归档中的一道坏题。

~~~text
Request
  -> Idea -> Statement -> Similarity
  -> Solution -> Data -> Judge -> Quality
  -> Package gates -> READY
~~~

生成模型负责提议，确定性 validator 负责裁决。

## 为什么这不是对 LLM API 的一层封装

真正有意思的工程在于：让一个不可靠的生成器既能安全使用，又能低成本重跑：

- **两阶段结算的预算账本。** 九个预算维度（模型调用、token、成本、查重调用、沙箱
  运行、制品字节数、阶段尝试次数、活跃墙钟时间）在不可逆工作*之前预留*，之后单调
  结算。每一次物理外部调用都带有稳定的逻辑身份和物理 `CallTrace`，因此重试与崩溃
  恢复对每个效应只结算一次。
- **不可信代码执行边界。** 生成的 C++ 只通过专用的 Docker runner 编译和执行：
  非 root 身份、丢弃 capabilities、只读根文件系统、显式挂载、pids/memory/CPU 限制、
  不暴露 socket。一个**分离的看门狗进程**会在超期或进程丢失时停止计划中的目标，
  而一个范围很窄的 reconciler 只能清理*恰好*是那些已持久化的资源身份。
- **分级保证的恢复。** 中途死亡的进程由阶段特定规则处理，而不是笼统的重试：若未
  授权任何外部效应则重跑；若已发出则重放或对账同一个稳定的 provider 身份；遇到
  未知的发送边界则保守计费并暂停。`READY` 永远无法从部分验证的状态到达。
- **内容寻址的制品存储。** 不可变的 SHA-256 blob 依次经过声明 → writer token →
  校验 → pin → occurrence 写入，把字节绑定到 run、修订、角色与生产者证据。
- **贯穿全局的一条不变式：** SQLite 写事务内部绝不发生任何外部 I/O。provider 调用、
  Docker 调用、哈希计算、fsync 与阻塞等待全部发生在事务之外；短事务原子地提交
  投影与效应。

## 快速开始

需要 Go 1.25.0 或更高版本，以及本地 Docker Engine。

~~~bash
go build ./cmd/...

# 1. Validate your configuration without touching the network.
#    `validate` takes no flags; `effective` requires --redact.
./cpgen --config config/mvp.example.yaml config validate
./cpgen --config config/mvp.example.yaml config effective --redact

# 2. Confirm the host, Docker engine and toolchain are usable.
#    The API version is pinned; image IDs come from config/toolchains/docker-v1.lock.json.
./cpgen doctor --json --engine-endpoint unix:///var/run/docker.sock \
  --api-version 1.55 \
  --builder-image  sha256:fe432330efb137a6d713a05de0c5310a6736a23f1882612456cb40283ca1f107 \
  --runtime-image  sha256:d4cbcfb1c9cf9de450b2f5296a9fce8631992608c878ccae0e69edffaa17f2b5 \
  --transfer-image sha256:875576235bfbfa8ecd995175ee078beca2afae16ab99cd8dca9bfce726904b6a \
  --execution-protocol docker-direct-v2

# 3. Start a run (foreground; one process per run).
./cpgen --config config/mvp.example.yaml generate --request config/mvp.request.yaml

# 4. Inspect, cancel, or resume it.
./cpgen --config config/mvp.example.yaml run list
./cpgen --config config/mvp.example.yaml run show   RUN_ID
./cpgen --config config/mvp.example.yaml run events RUN_ID
./cpgen --config config/mvp.example.yaml run resume RUN_ID
./cpgen --config config/mvp.example.yaml run cancel RUN_ID

# 5. Export the verified package, then re-verify it independently.
./cpgen --config config/mvp.example.yaml run export RUN_ID --output ./problem.zip
~~~

从 `config/` 复制一个 `*.example.yaml`，替换其中的路径、endpoint，以及全零的
toolchain lock 摘要。参见 [config/README.md](config/README.md)。

错误是带类型且机器可读的，配置问题会在任何网络或 Docker 工作开始之前被捕获：

~~~console
$ cpgen --config config/mvp.example.yaml config validate
{"schema_version":"cpgen.cli/v1","status":"ERROR","error":{"code":"config_invalid",
 "message":"sandbox.toolchain_lock_path: must be absolute",
 "field":"sandbox.toolchain_lock_path"}}
~~~

每条命令都把失败映射为**稳定的退出码**和稳定的错误 `code` 值，因此脚本可以驱动
CLI，而无需解析自然语言输出。

业务结果来自 run 的状态，这让脚本能够区分“这道题需要人工介入”与“工具坏了”：

| 退出码 | run 状态 |
|---:|---|
| 0 | `READY`（或另一条成功路径） |
| 5 | `BLOCKED` |
| 6 | `NEEDS_REVIEW` — 等待人工决策 |
| 7 | `FAILED` |
| 8 | `CANCELLED` |

操作类失败带有独立的退出码和稳定的 `error.code`：

| 退出码 | 含义 | `error.code` |
|---:|---|---|
| 2 | 输入格式错误或配置无效 | `config_invalid` |
| 3 | run 或对象不存在 | `not_found` |
| 4 | 另一个进程持有该 run 的锁 | `lock_busy` |
| 5 | 无效的状态转换 | `invalid_state` |
| 7 | 并发版本冲突 | `version_conflict` |
| 9 | 未分类的操作失败 | `operation_failed` |
| 10 | 沙箱清理仍未完成 | `cleanup_pending` |

### 你会得到什么

`run export` 生成一个 `cpgen.package/v2` 归档，评测系统可以直接消费：

~~~text
problem.zip
  statement/statement.md
  solution/reference.cpp
  solution/brute.cpp
  judge/generator.cpp
  judge/validator.cpp
  judge/checker.cpp
  data/tests.json
  tests/01.in  tests/01.ans
  ...
  report/quality.json
  report/provenance.json
~~~

该归档中的参考题解已针对每个测试编译并执行，暴力题解已做过差分比对，并且整个归档
都可以通过一次独立的 CLI 调用重新校验。

### 查看进行中的 run

每条读取命令默认输出稳定、带版本的 JSON 信封，因此前端或仪表盘可以直接消费——
CLI 并不是唯一可能的客户端。`run show RUN_ID` 打印当前投影：

~~~json
{"schema_version":"cpgen.cli/v1","status":"RUNNING","data":{
  "run_id":"run_ca074cfc318cd51f78b005a09eff8451",
  "state":"RUNNING","version":86,
  "workflow_revision":"mvp.idea.statement.similarity.solution.data.judge.package.v1",
  "current_stage":"solution","current_stage_ordinal":5,
  "request_digest":"sha256:b77be0c25168ec5768cedefa330179beb12fb46f2f88b72c57ab4fe078551573",
  "config_digest":"sha256:38f0cc4eca50edf3472bb9593401d550f479576b186003d368e1360f0b9576cd",
  "active_elapsed":68156221200}, "run_version":86}
~~~

`run events RUN_ID` 流式输出只追加的审计轨迹，`review show RUN_ID` 报告待处理的
人工决策。

## 架构

~~~text
                 +-----------------------------+
   cpgen CLI --> |  application coordinator    |  fixed typed stage loop
                 |  run lock / attempts /      |  (compiled in, not a runtime
                 |  review / cancellation      |   graph, not a hosted service)
                 +--------------+--------------+
                                |
        +-----------------------+-----------------------+
        |                       |                       |
   +----v-----+          +------v------+         +------v-------+
   | execution|          |  artifact   |         |  adapter/    |
   | metered  |          |  content-   |         |  sandbox/    |
   | LLM +    |          |  addressed  |         |  docker      |
   | similar. |          |  blob store |         |  + watchdog  |
   +----+-----+          +------+------+         +------+-------+
        |                       |                       |
   +----v-----------------------v-----------------------v-------+
   |  SQLite: projections + ledgers (budgets, calls, artifacts,  |
   |          sandbox resources, reviews, packages)              |
   +-------------------------------------------------------------+
~~~

阶段 1 的范围被刻意压得很小：**一台主机、一个私有工作区、每个 run 一个前台执行器、
一条编译进二进制的固定流水线，以及作为权威来源的 SQLite。** 没有守护进程、任务队列、
远程 worker，也没有任意运行时图。不同 run 可以在各自独立的进程中并发推进；单个
run 永远不会有两个会修改状态的执行器。

完整系统契约：[ARCHITECTURE.md](ARCHITECTURE.md)。
各项决策及其权衡：[docs/adr](docs/adr)。
各组件细节：[docs/design](docs/design)。
塑造了本设计的两个问题：[docs/DESIGN-NOTES.md](docs/DESIGN-NOTES.md)。

## 值得一读的设计决策

每一条都记录了一条*未*走的路，而这通常是信息量更大的那一半：

| ADR | 决策 |
|---|---|
| [0001](docs/adr/0001-static-typed-workflow.md) | 静态类型流水线与 activity 契约 |
| [0002](docs/adr/0002-run-state-review.md) | 七种 run 状态、评审、重试、取消、重启 |
| [0003](docs/adr/0003-judge-outcomes.md) | 评测结果与确定性优先级 |
| [0004](docs/adr/0004-docker-direct-execution.md) | 在隔离的 Docker cgroup 中直接执行目标 |
| [0005](docs/adr/0005-docker-execution-lifecycle.md) | 执行生命周期、看门狗、跨停止传输 |
| [0006](docs/adr/0006-lightweight-local-workflow.md) | 轻量本地工作流边界 |

ADR-0006 已经过修订，用本地固定循环取代了早先基于 LangGraph 的调度器，从而保持
SQLite 作为权威来源，并防止 provider 库类型泄漏进领域契约。

## 验证

本仓库中的正确性主张由 [docs/evidence](docs/evidence) 中记录的证据支撑，而不是靠
断言。发布门禁在 CI 中强制执行：

~~~bash
go test ./...
go vet ./...
go test -race -timeout 30m ./...
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
git diff --check
~~~

CI（`.github/workflows/ci.yml`）还会在 Go 1.25.0 和当前稳定版上验证 `go mod verify` /
`go mod tidy` 的整洁性、`gofmt`、针对文档化文件集的可执行架构一致性检查，以及
Linux 与 Windows 交叉构建。

测试覆盖了这类系统真正要紧的失败路径：每个持久化阶段边界上的进程死亡、双进程锁
互斥以及异常终止后的释放、未知的 provider 发送边界、Blob 损坏与路径穿越、看门狗
死亡与超期处理，以及“SQLite 写事务内不发生任何外部 I/O”这一断言。需要真实 Docker
Engine 或付费 provider 的测试是可选的，并有明确标记，因此默认测试套件是确定性的、
离线的。

## 项目状态

Slice 0（执行基础）、Slice 1（lightweight local workflow，轻量本地工作流），以及 MVP
生成循环——查重 ACCEPT → 题解 → 数据 → Docker/评测 → 质量 → 题包 → `READY`——均已
完成，并通过了针对一道普通 C++ 题目的真实 Docker 验收与独立 CLI 验收，其中包括
事务崩溃恢复和从导出的 ZIP 重新编译。

已知限制，如实列出：

- 查重/原创性检查目前针对一个**本地 fixture**运行，因此相对于真实题库的原创性
  尚未确立。
- 特殊评测（SPJ）、题解最小化，以及通用的不可信导入执行不在当前 MVP 范围内。
- 自动题目变异被推迟；未通过验收的业务结果按设计停下来等待人工评审。
- 完整循环的验收证据针对普通 C++ 题目；Go 端到端验收与外部服务可用性仍属后续工作。

[docs/development-log.md](docs/development-log.md) 记录了开发顺序，
[docs/traceability.md](docs/traceability.md) 把每条需求映射到其设计与支撑证据。

## 仓库结构

| 路径 | 内容 |
|---|---|
| `cmd/` | `cpgen`、`cpgen-image-lock`、`cpgen-transfer` 可执行文件 |
| `internal/domain` | 带版本的不可变领域值与契约 |
| `internal/port` | 各层之间的能力接口 |
| `internal/application` | run 协调与固定类型的阶段循环 |
| `internal/execution` | 计量式 LLM/查重调用、重试、回执、缓存 |
| `internal/adapter` | Docker 沙箱、SQLite 存储、blob 存储 |
| `internal/cli` | 命令界面、进度事件、退出码 |
| `docs/` | ADR、组件设计、证据、计划 |
| `config/` | 示例配置与 toolchain lock |

## 许可证

MIT — 见 [LICENSE](LICENSE)。
