# 架构返工记录（2026-09-14）

用户退回了第一版 R1–R4：行为测试虽然通过，抽象、转发和装配成本却增加。此次按“修改一个阶段需要理解的东西更少”完成返工和验证；[原方案](../design/architecture-follow-up-2026-09-14.md)仅作为历史分析。

## 实际删减

比较对象是用户退回时的工作区，不是 Git HEAD；没有重置或覆盖此前未提交工作。统计范围为 `internal/application` 中非 `_test.go` 的 Go 文件，行数包含注释和空行，类型按顶层 `type` 声明计数。

| 项目 | 返工前 | 返工后 | 净减少 |
|---|---:|---:|---:|
| 生产文件 | 86 | 68 | 18 |
| 类型声明 | 170 | 149 | 21 |
| 其中接口 | 39 | 34 | 5 |
| 生产代码行 | 12676 | 12223 | 453 |
| 全部应用层 Go 行（含测试） | 25637 | 25201 | 436 |

删除 lifecycle、termination、cleanup、attempt registry、review/recovery handler 包装对象及恢复回调注册表。LocalRunService 直接拥有运行状态和完成/恢复操作；stageControl 仍负责 poller 的启动与 join，fixedStages 只处理具体业务阶段。

删除五个与唯一 Reader 实现同形的证据接口、四套一次性阶段配置及其构造器。阶段直接持有自己的具体 Reader，通过方法取得已验证结果；不持有上游 Executor，也不穿透其他 Reader 的字段。外部调用用 `Reader()` 明确选择读取入口，不再为每个读取方法写转发。

合并重复的 MVP/通用装配配置，删除 Resources DTO 与 StagePublicationScope。现有 attempt 和 version 可直接用于发布；生产装配在 `GenerationRunConfig` 校验共享资源之后直接构造阶段。历史 Slice 构造器仅留作测试适配。删除不起作用的 StorageFactory 注册 API 和中间资源包装。

把每个阶段的 Reader 定义和证明核验放在同一文件。当前主要入口：[装配](../../internal/application/generation_run_service.go)、[协调用例](../../internal/application/run_service.go)、[阶段事务](../../internal/application/run_stage.go)、[恢复与取消](../../internal/application/run_recovery.go)、[业务分派](../../internal/application/fixed_stages.go)、[Package 读取](../../internal/application/package_reader.go)。

## 保留的边界

固定循环和 SQLite 进度权威、离线读取、不可变 workflow 定义、只读 LLM/sandbox 证明策略及受控发布仍保留。存储读取接口是实际权限边界，不按实现一比一生成接口。

原 revision、schema、canonical digest、物理调用/receipt/writer 身份、预算及数据库 migrations 未改。READY 仍通过 FinalizeVerifiedPackage 原子提交。取消仍先停止并 join、结算和精确清理，再提交终态；缓存仍是提交后的可选索引。

账本中的 attempt、call、reservation、writer、receipt、occurrence 分别表达故障窗口；本轮封装其使用，没有合并持久化事实。RunView、BudgetSnapshot 和 CallTrace 仍是读取视图。

## 返工验证

环境为 Windows amd64、Go 1.26.5；真实 Docker 使用本机固定工具链文件。没有调用付费模型；供应商结果使用本地 HTTP fixtures。

| 检查 | 本次返工结果 |
|---|---|
| `go test ./... -count=1 -timeout 15m` | PASS；application 48.634s、sqlite 30.422s、integration 5.600s |
| `go vet ./...`、`git diff --check` | PASS |
| Linux command cross-build（CGO_ENABLED=0） | PASS |
| Go 1.25 全量（无缓存） | PASS；application 93.043s、sqlite 65.583s |
| `go test -race ./... -count=1 -timeout 30m` | PASS；application 536.696s、sqlite 622.834s、integration 124.177s；无 race 报告 |
| 真实 Docker 正常路径恢复及证据替换拒绝 | PASS；150.23s，保留原 attempt，未重复计费 |
| 真实 Docker MVP CLI | PASS；177.41s，事务中断恢复、READY、无凭据离线导出及 ZIP 独立执行 |
| 真实 Docker 中断取消 | PASS；6.38s，unsealed_source/compile_receipt 两个中断窗口，无重新发送 |
| 文档链接、架构一致性 | PASS；132 个本地链接、27 份规范文档 |

Docker 命令顺序执行，未与全量编译或 race 并行：

```powershell
$env:CPGEN_RUN_DOCKER_CANARY='1'
$env:CPGEN_DOCKER_TOOLCHAIN_LOCK='D:/cpgen-private/toolchains/docker-v1.lock.json'
go test ./internal/application -run '^TestDataRunServiceRequiresRealPassingSolutionAndPreservesDraft$/^pass$' -count=1 -timeout 20m -v
go test ./internal/application -run '^Test(MVPPublicCLIResumesDataDraftAndExportsVerifiedPackage|SolutionRunServiceCancelsInterruptedVerificationWithoutRedispatch)$' -count=1 -timeout 20m -v
```

首次返工 Docker 正常流程完成后，证据篡改测试的装配被拒绝（159.24s）：测试存储包装是含函数字段的不可比较值，而既有资源校验要求同一实例。四处测试包装改为共享指针，生产身份检查和篡改断言均保持；MVPComposition/SolutionExecutor 定向回归通过，随后正常路径、MVP CLI、取消均通过。仅测试包装身份发生修正，生产代码与已通过的全量/race/Go1.25 验证版本相同。

当前普通阶段的输入、执行、提交和恢复选择都在固定分派中修改，通用预算、writer 和计时协议无需认识具体业务阶段。现有测试的断言保持：调用次数、原 attempt/物理身份、预算、错 run/attempt、缺失或替换的证据、清理及 READY 绑定均需满足。

## 既有记录与限制

前一版 R1–R4 的普通/Go1.25/race 检查通过，但结构被用户否决，不能据此作为本次返工通过证据。前一版 Docker 组合测试曾在 Data/pass 进入非预期 Judge 复核；正常路径单独复跑通过，原因未证实。新增的 Data/Judge 失败诊断保留，本次容器检查避免与全量编译及 race 并行。

工具链快照和专用 publication 账本不混入这次返工。离线导出仍需原冻结工具链文件存在且摘要匹配；缺失时明确拒绝。未创建提交、部署或清理用户数据。


## 后续 workflow 清理

用户指出 workflow 中仍有开发计划代码后，再次检查生产引用。`Slice2Pipeline`、`Slice2Output`、`StagePolicyProvider` 及整条串联执行/校验/恢复实现仅由其自身测试使用，已删除；对应测试一并删除。历史 run 的恢复本来就由应用层固定循环处理，保留这些执行器并不能提供额外数据兼容。此前基线中的“为自身契约测试保留”结论已纠正。

- [流程定义](../../internal/workflow/definition.go)只描述当前生成流程、Fake 流程及兼容选择；[revision 兼容表](../../internal/workflow/revisions.go)保存原始字符串与历史阶段序列。
- 默认流程改为 [fake.Pipeline](../../internal/adapter/fake/pipeline.go)，其能力配置归入 fake 适配器；删除重复构造校验和 RunPrepare/RunExercise/RunCheckpoint 转发。对应域类型改为 FakeInput/FakePrepared/FakeEvidence/FakeCheckpoint，JSON 字段与摘要输入不变。
- 当前装配入口命名为 `NewGenerationRunService`，当前 revision 常量为 `GenerationRevision`；历史常量明确标记 `Legacy`，没有保留旧名别名。
- [兼容测试](../../internal/workflow/definition_test.go)以原始字符串锁定五种持久化 revision、完整阶段顺序和执行能力。现有协调器测试继续覆盖从各阶段恢复、失败投影、控制结果及并发隔离。

此项单独比较清理开始时的工作区：workflow 生产代码 **787 → 116 行**、类型 **10 → 2**；计入移至 fake 的文件，全部 internal 生产代码仍净减 **592 行、3 个类型**。这不是把删除数量和搬迁数量相加；上一节应用层返工指标仍对应上一次验证时间点。

本次验证：`go test ./... -count=1 -timeout 15m` 通过（application 43.267s、sqlite 27.488s、integration 5.861s）；`go vet ./...`、gofmt、`git diff --check`、27 份规范文档架构检查通过。定向 race 命令如下，三个包均通过（application 16.263s）：

```powershell
go test -race ./internal/workflow ./internal/adapter/fake ./internal/application -run 'TestDefinition|TestPipeline|TestCompiledGraph|TestMVPComposition|TestRunService' -count=1 -timeout 5m
```

首次全量检查发现批量导入更新给 similarity 测试误加了一个未使用的 fake import；已移除，随后重跑全量通过。本次没有修改 Docker、账本或数据迁移行为，未重跑真实 Docker canary、全量 race、Go 1.25 或 Linux 构建；前一节这些结果属于前一次返工验证。
