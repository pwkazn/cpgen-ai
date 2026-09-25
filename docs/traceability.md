# 需求与设计可追溯性

状态：ADR-0006 下为当前

Slice 1 与普通 C++ MVP 闭环：**完成**。完整 MVP 将 ACCEPT 路由到 Solution/Data/Docker/Judge/Quality/Package，未被接受的业务结果进入评审；旧 Slice 2 预览仍终止于不可豁免的评审。自动变异推迟以重新设计。见 [Slice 1 证据](evidence/slice1-verification.md)、[题包验收](evidence/mvp-package-commit-foundation.md)与 [V2 CLI 真实模型验收](evidence/v2-cli-live-acceptance-2026-09-15.md)。真实查重尚未验收。

## MVP 功能需求

当前新任务采用 V3；[执行样例设计](design/executed-samples.md)与 [2026-09-22 稳定性复验](evidence/ready-stability-2026-09-22.md)补充下表历史 V1/V2 检查点。验收覆盖三个真实模型任务和真实 Docker，Similarity 为本地 TLS fixture。

| 需求 | 决策/设计 | 验证 |
|---|---|---|
| 从结构化请求到已验证题包 | ARCHITECTURE 第 1、6、12 节；阶段 1 设计 | 固定流水线 E2E 与题包门禁测试 |
| 类型化阶段边界 | ADR-0001；工作流设计 | 编译期与源码边界测试 |
| 单宿主前台执行 | ADR-0006；CLI 设计 | 真实子进程命令测试 |
| 每个 run 一个会修改状态的前台执行器 | ADR-0006；工作流/存储设计 | 同 run 锁竞态与进程死亡释放 |
| 持久化暂停与人工恢复 | ADR-0002；工作流设计 | 每个阶段边界处的崩溃 |
| 人工评审 | ADR-0002；工作流/存储/CLI | 决策生命周期与陈旧绑定测试 |
| 及时响应的取消 | ADR-0002；工作流/CLI/沙箱 | 并发取消与目标停止证明 |
| 带结构化输出的模型调用 | LLM 设计 | 严格内容草稿、私有响应/诊断恢复、有界修复与本地领域绑定；[草稿证据](evidence/slice2-content-drafts.md) |
| LangChainGo 提供方边界 | ADR-0006 修订；LLM 设计 | 规范 HTTP 一致性、严格校验、持久化物理账本、精确私有重放、用量、取消与脱敏；[派发证据](evidence/slice2-durable-llm-dispatch.md)与[活动账本证据](evidence/slice2-active-llm-ledger.md) |
| 本地固定循环执行 | ADR-0006 修订；工作流设计 | 类型化串行路由、经过校验的生产提交、同尝试重启与取消；[架构简化证据](evidence/architecture-follow-up-2026-09-14.md)；早期图证据保留为历史 |
| 私有模型响应恢复 | LLM 设计；制品与预算协议 | internal/application/llm_replay_test.go 与 llm_replay_process_test.go：经验证的重放、不重发、预算/取消/提交失败；artifact_reader_test.go：已填充迁移与精确读取器绑定；完整检查点门禁通过 |
| 有界 JSON 格式修复 | LLM 设计；严格的提供方配置 | physical_repair_test.go：符合条件的脱敏诊断；llm_validation_test.go：私有失败回执；llm_structured_test.go 与 llm_structured_process_test.go：一次不可变修复、独立 trace/用量、预算/取消/崩溃恢复与原子挂载；完整检查点门禁通过 |
| 查重证据与策略 | 查重设计 | 持久化证据、已提交输入与策略路由已实现；[类型化证据](evidence/slice2-typed-similarity.md)与[闭环验收](evidence/mvp-package-commit-foundation.md)。本地 fixture 不代表真实查重或原创性通过 |
| 从被接受的查重到可用题包 | 当前生成闭环计划 LOOP-01/SOL-01 至 PKG-01 | 已完成：ACCEPT 进入真实 Solution/数据/Judge/题包；其他业务结果进入人工评审，且无自动变异；[导出题包 E2E](evidence/mvp-package-commit-foundation.md)与 [V2 CLI 验收](evidence/v2-cli-live-acceptance-2026-09-15.md) |
| 直接 Docker Judge 执行 | ADR-0004；沙箱设计 | Slice 0 加真实引擎测试 |
| CLI 丢失时的目标安全 | ADR-0005；沙箱设计 | 看门狗超期/EOF 与 kill 测试 |
| 不可变制品与来源 | 存储设计 | Blob/写入器/pin 与损坏测试；声明与缓存元数据绑定、已结算写入挂载且不重复计费；[制品证据](evidence/slice2-artifact-mutation-records.md) |
| 确定性题包与 READY | 题包设计 | 结构/语义门禁与同 run 约束 |
| V3 执行样例定稿与草稿答案隔离 | [执行样例设计](design/executed-samples.md)；program-context 与 finalized-statement 契约 | [V3 验收](evidence/ready-stability-2026-09-22.md)：错误草稿答案隔离、独立执行与差分失败拒绝、brute 超范围拒绝、定稿发布间隙恢复；三个真实模型 run 的 27 组答案独立核对 |
| 历史版本与恢复身份保持 | [执行样例版本边界](design/executed-samples.md#版本恢复与预算)；冻结配置与工具链快照 | [稳定性复验](evidence/ready-stability-2026-09-22.md)：固定提示/schema 摘要回归、历史包离线导出、新任务两次无凭据恢复及预算快照核对 |

## 非功能需求

变异核心/意图、结果账本、候选保留、原子变异完成与类型化初始批次发布保留其[历史证据](superpowers/plans/2026-09-09-slice2-business-routing.md)。其未完成的来源读取器/授权/闭环属于推迟的研究，不是 MVP 功能需求。

| 需求 | 设计机制 | 验证 |
|---|---|---|
| 写事务内无外部 I/O | 工作流与存储协议 | 事务插桩 |
| 保守的预算记账 | 预算与调用账本 | 并发预留与未知边界测试 |
| 稳定的幂等性 | 阶段尝试与调用身份 | 重复命令与重启测试 |
| 精确的 Docker 清理范围 | SandboxExecution 资源计划 | 无关资源拒绝测试 |
| 进程崩溃恢复 | 操作系统锁加领域账本 | 强制子进程终止套件 |
| 安全与隐私 | 严格配置、私有路径、适配器脱敏 | 路径、密钥、HTTP 与题包安全测试 |
| 可复现性 | 修订、规范 digest、镜像/工具链身份 | 确定性 fixture 与题包字节 |
| 可审计性 | run 事件、尝试、CallTrace、occurrence、回执 | 跨账本完整性测试 |
| 本地并发 | 每 run 锁加期望版本 | 同 run 冲突与不同 run 并发 |
| Go 发布质量 | 仓库门禁 | test、vet、race、交叉构建、补丁检查 |

## 切片检查点

| 切片 | 范围 | 证据 |
|---|---|---|
| 0 | 直接 Docker 执行、看门狗、Judge 基础 | docs/evidence/slice0-verification.md |
| 1 | 轻量本地工作流、持久化、账本、Fake 流水线、CLI | [Slice 1 验证证据](evidence/slice1-verification.md)与架构检查 |
| 2 | 请求、idea、statement、模型、查重 | [历史预览](evidence/slice2-live-preview.md)止于评审；完整 MVP 已接通正向路由，变异路由延期 |
| 3 | 普通 C++ 题解与 Docker Judge | [Solution](evidence/mvp-solution-foundation.md)、[Judge](evidence/mvp-judge-foundation.md)；SPJ 与 Go 实际闭环延期 |
| 4 | 数据与质量 | [Data](evidence/mvp-data-foundation.md)、[Quality](evidence/mvp-quality-package-foundation.md) |
| 5 | 题包与 E2E | [原子 READY、导出与独立复验](evidence/mvp-package-commit-foundation.md)；通用不可信包导入延期 |

## 架构边界检查

scripts/check-slice1-architecture.ps1 检查当前 ADR、组件设计、阶段 1 规格、架构、实施计划、可追溯性与 README。它强制使用已接受的轻量术语，并拒绝重新引入被取代的通用运行时机制的现行文档。被否决的备选方案仅记录在两份当前决策记录中，或明确划定的历史 ADR 说明中。

库修订新增了必需的进程内/权威存储文档检查。internal/workflow/source_boundary_test.go 在生产 Go 源码（包括嵌套目录）中强制提供方/调度器的导入位置。CI 运行两项检查、完整竞态测试与 vet，验证模块整洁性并交叉构建 Windows 与 Linux，且不启用付费提供方冒烟测试。
