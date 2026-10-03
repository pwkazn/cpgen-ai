# CPGen

CPGen 是一个用 Go 编写的竞赛编程题目生成命令行工具。它根据结构化请求调用语言模型，生成题面、题解和测试数据，在本地 Docker 中编译、执行和评测程序，最后导出 ZIP 题包。

当前支持普通 C++ 题目的完整生成流程，使用固定的 exact-token checker。默认示例配置采用 V3 工作流，通过参考程序与暴力程序的执行及差分检查定稿样例答案。

```text
请求 → 创意 → 题面 → 查重 → 题解 → 数据 → 评测 → 质量检查 → 题包
```

## 环境要求

- Go 1.25.0 或更高版本。
- 本地 Docker Engine，以及按工具链锁准备的构建、运行和传输镜像。
- 符合配置契约的模型服务和查重服务，以及相应凭据。
- 用于保存 SQLite 状态和生成制品的本地私有目录。

完整配置和工具链准备步骤见 [配置说明](config/README.md)。示例中的服务地址、模型、路径和工具链摘要是占位配置，需要先替换。

## 安装与配置

在仓库根目录构建：

```bash
go build -o cpgen ./cmd/cpgen
```

Windows 使用 `go build -o cpgen.exe ./cmd/cpgen`，后续命令中的 `./cpgen` 对应 `./cpgen.exe`。

复制 [完整工作流示例](config/mvp.example.yaml) 到自己的本地配置文件，例如 `.local/cpgen.yaml`，并设置：

- 模型与查重服务的 endpoint、服务身份和凭据环境变量。示例引用 `CPGEN_LLM_API_KEY` 与 `CPGEN_SIMILARITY_API_KEY`，不要将密钥写入仓库。
- `storage.state_root`：本地私有目录的绝对路径。
- `sandbox.engine_endpoint`：Linux 使用 `unix:///var/run/docker.sock`，Windows 使用 `npipe:////./pipe/docker_engine`。
- `sandbox.toolchain_lock_path` 与 `sandbox.toolchain_lock_digest`：实际工具链锁的绝对路径及规范摘要，替换全零占位符。

检查配置：

```bash
./cpgen --config .local/cpgen.yaml config validate
./cpgen --config .local/cpgen.yaml config effective --redact
```

`config validate` 只检查配置内容，不连接网络或 Docker，也不验证工具链锁文件。生成启动时还会检查锁摘要、本地 Docker 能力和已安装镜像。主机诊断命令 `doctor` 的参数见 [CLI 说明](docs/design/cli.md)。

## 生成题目

参考 [请求示例](config/mvp.request.yaml) 设置题目要求。预算只需填写 `max_llm_input_tokens` 和 `max_llm_output_tokens`，分别限制本次任务累计的输入、输出 token，包含重试和重新生成；示例为 150000 / 50000，两项额度独立。然后运行：

```bash
./cpgen --config .local/cpgen.yaml generate --request config/mvp.request.yaml
```

生成在前台执行，会调用已配置的外部服务并消耗请求预算。记下命令返回的 `RUN_ID`，后续查询、恢复和导出都使用该标识符。

```bash
# 查看任务列表、当前状态和事件
./cpgen --config .local/cpgen.yaml run list
./cpgen --config .local/cpgen.yaml run show RUN_ID
./cpgen --config .local/cpgen.yaml run events RUN_ID

# 恢复中断的任务，或取消任务
./cpgen --config .local/cpgen.yaml run resume RUN_ID
./cpgen --config .local/cpgen.yaml run cancel RUN_ID --reason "手动取消"

# 查看待处理的人工决策
./cpgen --config .local/cpgen.yaml review show RUN_ID
```

将 `RUN_ID` 替换为实际标识符。恢复时需要保留创建任务时的有效配置、路径和工具链。每个 run 同时只允许一个修改状态的执行器；不同 run 可以由各自的进程执行。

CLI 使用带版本的 JSON 输出，并以退出码区分结果：

| run 结果 | 退出码 | 含义 |
|---|---:|---|
| `READY` | 0 | 题包已通过当前门禁，可以导出 |
| `BLOCKED` | 5 | 任务受阻 |
| `NEEDS_REVIEW` | 6 | 需要人工决策 |
| `FAILED` | 7 | 任务失败 |
| `CANCELLED` | 8 | 任务已取消 |

其他命令也可能以相同退出码报告操作错误，脚本应同时检查 JSON 中的状态和 `error.code`。完整命令及错误约定见 [CLI 说明](docs/design/cli.md)。

## 导出题包

当任务达到 `READY` 后：

```bash
./cpgen --config .local/cpgen.yaml run export RUN_ID --output ./problem.zip
```

导出会重新检查已提交的题包证据。目标目录必须存在且支持硬链接；目标文件已存在时，命令会拒绝覆盖。

当前 V3 工作流导出 `cpgen.package/v3`，历史任务保留原有题包版本。归档的主要内容如下：

```text
manifest.json
statement/statement.md
statement/samples.json
solution/editorial.md
solution/reference.cpp
solution/brute.cpp
judge/generator.cpp
judge/validator.cpp
judge/checker.cpp
data/tests.json
tests/*.in
tests/*.ans
reports/similarity.json
reports/prepackage-quality.json
reports/provenance.json
```

题包格式见 [题包设计](docs/design/package.md)。接入具体评测系统需要适配其导入格式；当前没有通用题包导入或独立复验命令。

## 当前限制

- `READY` 表示编译、执行、差分和题包检查等既定门禁通过，不是题意或算法正确性的证明。有限测试可能遗漏参考程序与暴力程序的共同错误，题面和题解仍需人工审阅。
- 完整流程的验收针对普通 C++ 题目；SPJ、Go 端到端验收和通用的不可信程序导入执行尚未覆盖。
- 现有端到端验收中的查重服务使用本地 TLS fixture，尚未验证真实题库查重效果或原创性。
- 测试计划中的 small/boundary/stress 分类不代表逐约束覆盖证明；当前样例解释是输出说明，不是算法逐步推演。

当前默认工作流及验收记录见 [执行样例设计](docs/design/executed-samples.md) 和 [V3 稳定性验收](docs/evidence/ready-stability-2026-09-22.md)。

## 开发

项目采用本地固定流水线与 SQLite 持久化状态，每个 run 使用独立的进程锁。Slice 1 lightweight local workflow 的边界与后续生成阶段共同遵循 [架构契约](ARCHITECTURE.md)。

| 路径 | 内容 |
|---|---|
| `cmd/` | CLI 与工具链辅助命令 |
| `internal/domain/` | 领域值与契约 |
| `internal/port/` | 能力接口 |
| `internal/application/` | run 协调与阶段执行 |
| `internal/execution/` | 模型、查重调用及计量 |
| `internal/adapter/` | Docker、SQLite 与制品存储 |
| `internal/cli/` | 命令、输出和退出码 |
| `config/` | 示例配置与工具链锁 |
| `docs/` | 设计、决策和验收记录 |

本地检查：

```bash
go test -race -timeout 30m ./...
go vet ./...
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
git diff --check
```

默认测试不需要付费服务；真实 Docker 或 provider 集成测试需要另行启用。[CI](.github/workflows/ci.yml) 还检查模块一致性、格式，以及 Linux/Windows 构建。

## 文档

- [文档索引](docs/README.md)
- [配置与工具链](config/README.md)
- [CLI 命令](docs/design/cli.md)
- [系统架构](ARCHITECTURE.md)与[架构决策](docs/adr)
- [需求与实现映射](docs/traceability.md)
- [验收证据](docs/evidence)

## 许可证

[MIT](LICENSE)
