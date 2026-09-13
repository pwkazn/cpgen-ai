# 当前产物打包、READY 事务与导出入口

2026-09-10：内部 MVP 的真实 Docker 正向 READY、打包提交中断恢复和归档读取已完整通过（226.146s）。完整 MVP 配置、独立 CLI、真实事务内进程退出恢复及导出后执行复验已通过（208.994s）；旧 Solution selector 保持原有边界。最终全量常规测试、vet、Linux 编译、架构检查与真实 Docker 失败路径回归通过；全量 race 已通过（SQLite 706.875s）。

## 实现

- `PackageExecutor` 重建当前已提交的 Solution、Data、Judge、Quality 和 Similarity 链，组装 v2 题面、解题说明、源码、固定 checker、全部输入/答案及 seed/test 索引。归档写入复用确定性本地产物计量和恢复协议。
- ZIP 使用固定元数据、排序与 Store 编码。读取不解压到文件系统，检查成员、路径、大小、摘要和规范编码；额外、重复、缺失、路径穿越、链接及尾随内容不能通过。
- M26 保留历史 run 字段、预算及引用，新增不可变 `verified_packages`。专用事务同时提交包 occurrence、package stage 和 READY。通用 `FinishStage` 仍拒绝 READY；当前 Quality、版本、取消、包预算、未结算调用及清理状态都参与检查。
- `ReadArchive` 将 READY 的 occurrence、包记录与重建后的当前题包逐字节比较。`run export RUN_ID --output PATH` 通过完整写入并同步的临时文件和排他硬链接发布，拒绝覆盖已有目标。

## 已获得的证据

- 归档往返、确定性和篡改测试通过。
- SQLite 包事务测试通过：注入提交失败后 occurrence/包记录/run 全部回滚；重试不改变包引用；相同幂等键不能接受不同内容；质量变化、取消请求和普通 READY 声明被拒绝；已提交记录不可修改或删除。
- 导出文件并发发布只允许一个成功者，不混合数据、不覆盖现有文件、不残留本轮临时文件；链接目标保护测试已加入。
- 定向 race：CLI 1.644s、packageprobe 1.809s、SQLite 22.092s。SQLite 全包测试 48.007s；vet、26 项架构检查和 diff 空白检查通过。随后完整 `go test ./...` 通过（SQLite 68.443s、application 84.495s、CLI 2.403s）。

## 验收配置与历史故障

`config/mvp.example.yaml` 和带显式预算的 `config/mvp.request.yaml` 提供完整固定图。Bootstrap 接通 Data 格式修复策略。独立验收先由本地 HTTP/TLS fixture 产生已提交 Data 草稿，然后用未修改 CLI 完成后续 Docker 阶段及导出。事务内真实退出由测试二进制注册的 SQLite 函数触发，生产代码没有新增故障环境开关。

早期失败包括 fixture 的零包预算、一次 SQLite busy、Docker Engine 管道不可用，以及测试局部变量遮蔽旧注入错误。正向预算和测试变量已修正，用户重启 Engine 后完成了下述通过验收；这些历史失败本身不计作通过。原工具链锁未被覆盖，真实 Docker 使用独立开发锁。

独立 CLI 测试还发现父进程 fixture 与配置的清理时限不一致，严格收据验证正确拒绝；已统一有效配置。导出曾误用要求目标已存在的配置路径解析，已改为绝对输出路径。一次 watchdog CLEANED 确认管道关闭也导致失败；没有放宽确认门禁、忽略清理错误或反复重启 Docker，后续稳定执行才计入通过结果。

## 最新通过结果

`TestMVPPublicCLIResumesDataDraftAndExportsVerifiedPackage` 已通过（208.994s）。测试验证了未修改 CLI 完成剩余 Docker 阶段；测试子进程在 SQLite 包事务内以 97 退出；数据库恢复后 run 保持 RUNNING、package occurrence 和 VERIFIED 记录均为 0、原 package attempt 保留；未修改 CLI 再恢复为 READY。最终仍只有 4 次本地模型请求、1 次本地 Similarity 请求和 96 次生成流程容器创建。

CLI 导出的 ZIP 与 VERIFIED 记录一致，重复导出拒绝覆盖。复验使用独立存储与执行身份，只读取 ZIP，重新编译 Reference/Brute，运行全部正式用例及 small 对拍并核对答案。它没有使用原 run 的程序或源 blob。范围是 C++ 普通题；这些本地 HTTP/TLS fixture 不代表已验证外部供应商或真实查重服务。
真实 Docker 失败路径回归通过（327.428s）：`wrong_answer`、`invalid_generated`、`nondeterministic`、`differential_wa`、`reference_tle` 均按预期停下并保留证据，不能产生 READY 包。日志位于 `.tmp/mvp-final-negative-docker.log`；独立 CLI 通过日志为 `.tmp/mvp-public-cli-crash.log`。临时日志不是源码验收前提，测试可由源码与显式 Docker 配置重跑。
## 重跑入口

常规门禁：`go test ./...`、`go test -race -timeout 30m ./...`、`go vet ./...`、`scripts/check-slice1-architecture.ps1`，以及 Windows/Linux 的 `go build ./cmd/...`。Docker 测试需显式设置 `CPGEN_RUN_DOCKER_CANARY=1` 和指向本机已构建锁文件的绝对路径 `CPGEN_DOCKER_TOOLCHAIN_LOCK`；未启用时普通套件会跳过实际 Docker 场景。

```powershell
go test ./internal/application -run '^TestDataRunServiceRequiresRealPassingSolutionAndPreservesDraft$' -count=1 -timeout 20m
go test ./internal/application -run '^TestMVPPublicCLIResumesDataDraftAndExportsVerifiedPackage$' -count=1 -timeout 20m
```

第一条包含正向恢复及五个失败分支；第二条覆盖公共 CLI、事务内真实退出、导出和独立复验。使用现有本地锁与已安装镜像，不覆盖原锁文件；这些 fixture 测试不需要外部供应商凭据。
## 最终 race 回归中的测试修正

首轮全量 race 的应用层通过（793.125s），CLI、包格式及其他套件也通过；SQLite 唯一失败是 `TestMigrationSimultaneousFirstOpenIsIdempotent`。26 个迁移在 race 下超过 fixture 的 5 秒锁等待，第二个打开者正确收到 `storage_busy`；原测试先 Fatal 后注册 Close，还造成失败路径文件句柄未关闭。修正仅限测试：明确 1 分钟上下文和 30 秒有界锁等待，并在所有退出路径释放 barrier、等待 worker、关闭已成功打开的 store。生产忙等待、迁移事务及错误映射未改变。

该场景 race 连续三次通过（23.952s），普通定向测试通过（1.750s），SQLite vet 与 diff 检查通过。首轮失败日志保留在 `.tmp/mvp-final-race.log`；修正后的完整门禁另记 `.tmp/mvp-final-race-recheck.log`。
本轮环境：Windows/amd64，Go 1.26.5，Docker Engine 29.7.2；分支 `codex/phase2`，基线 `50deb38599b1d825ea15b7bcf1140882b32ba7e8` 上的当前未提交工作树。Docker 使用独立 `.tmp/mvp-docker-v1.lock.json`，规范摘要 `sha256:4ba96cd4de6d1a06acc14821dd9a681adc6cda86381004f6b7f4a2170a8b0cce`。此记录不声称 CI 的 Go 1.25 或 Linux 真实 Docker 已实际执行；Linux 已完成交叉编译。
最终 `go test -race -timeout 30m ./...` 退出码为 0，SQLite 全套复跑通过（706.875s），此前通过且未受测试文件改动影响的包使用有效缓存。完整常规测试、vet、依赖完整性、Windows/Linux 命令构建、400 个 Go 文件格式和 26 项架构检查通过。最终检查完成，无遗留运行中的验收命令。普通 C++ 出题闭环的 P0 清单完成；既有 SPJ、变异、通用导入执行等后续任务不纳入本次完成声明。