# ADR-0006：轻量本地工作流

Status: Accepted

Date: 2026-08-31

Amended: 2026-09-08（进程内库集成）

Amended: 2026-09-13（本地固定调度）

Amended: 2026-09-26（本地 Web serve 与任务 goroutine）

## 背景

阶段 1 需要一条可恢复的产品工作流，但其部署边界是一台宿主和一个本地项目工作区。已完成的 Slice 0 Docker 看门狗、测量、预算与制品证据已经提供了困难的外部效果安全边界。构建托管式通用工作流运行时会引入产品并不需要的机制。

## 决策

决策：一台宿主上每个 run 一个前台 Go CLI 执行器；操作系统进程锁；固定的类型化流水线；SQLite 投影加 CPGen 领域账本；无托管运行时。

**2026-09-26 Web 执行方式修订：**同一个 `cpgen` 二进制增加前台 `serve` 子命令。Web 模式由一个 Go 进程承载多个 run goroutine，每个 goroutine 直接调用 `internal/application`；它不启动每 run 的 CLI 子进程，也不经 CLI 文本协议调用。服务内轻量管理器只负责并发容量、run 登记、独立任务 context 和退出等待。CLI 原有执行方式保留。任一模式下每个 run 仍最多一个修改状态的执行器，继续使用 per-run OS 锁协调 serve 与独立 CLI；SQLite 仍是唯一权威状态。没有队列或自动恢复，服务重启后由操作者显式恢复。

serve 在启动时验证配置并只监听 loopback。读取路由使用本地装配，不要求 Docker 可用；依赖检查留在真实生成/恢复路径。静态 UI 随二进制嵌入，不提供动态配置和 Web doctor/diagnostics 接口。

规范性契约使用以下确切短语：one foreground executor per run、per-run process lock、fixed pipeline 和 no workflow-hosting service。

协调器打开本地存储，获取确定性 run 锁，选择当前编译阶段，记录一次阶段尝试，在写事务之外调用外部工作，原子地提交投影与证据，并在暂停或终态处退出。不同的 run 可以并发使用不同的 CLI 进程。

七个 run 状态是 CREATED、RUNNING、BLOCKED、NEEDS_REVIEW、READY、FAILED 和 CANCELLED。重启是人工的，会重跑或对账当前领域阶段。重试在该阶段内部有界。

## 取代

Supersedes: 仅在 ADR-0001 暗示通用 Step 运行时之处取代 ADR-0001；取代 ADR-0002 的执行模式/租约/探测/恢复机制；取代旧的 Slice 1 持久化引擎范围。

该决策还拒绝守护进程、任务队列、任意运行时图、执行租约、租约纪元、所有者纪元、栅栏令牌、探测中或静默中的 run 模式、观察票据或观察下限、通用恢复意图、启动清理器以及分布式所有权。Temporal、AutoGen、CrewAI 与托管式 LangGraph 服务仍然没有必要；未来的跨宿主需求需要单独的 ADR 与需求证明。

## provider 库与本地调度

2026-09-13 的架构简化用 `internal/application` 中的本地固定循环取代了 2026-09-08 引入的 LangGraphGo 包装器。该包装器不拥有检查点、重试或恢复；那些已经属于 CPGen。该循环选择当前持久化的阶段，调用一个类型化阶段边界，并在继续之前校验其已提交的状态转移。它不引入新的工作流修订号或存储身份。历史库证据仍属历史。

LangChainGo `v0.1.14` 仍被固定用于 `internal/agent` 中的 provider 适配。最低 Go 版本为 1.25.0。库类型不得进入 `internal/domain`、`internal/port` 和 `internal/workflow`。SQLite 保持权威：进度由投影与经过验证的 Blob occurrence 重建。不接纳第二个 graph.json 存储、自动的库检查点持久化、包含私有状态的回调、库管理的重试或用户可配置的图。

传输重试使用现有的 CallCoordinator，每次授权对应一次物理派发。JSON 格式修复是一个独立的有界操作（启用时最多一次）；自动业务修复与 Idea 变更仍然推迟。调度器完成绝不授予 READY：全部八个业务阶段与同一 run 的题包事务仍然必需。显式配置选择已完成的普通题目 MVP；历史 Fake 与预览修订保留其既有边界。

## 保留

Retains: 类型化输入/输出、不可变 RunView、受限端口、ReviewDecision、Docker 看门狗/资源身份、预算、CallTrace、Blob/occurrence、题包门禁。

具体而言：

- Slice 0 的分离式看门狗与精确 Docker 授权身份仍然强制；
- 不可变的内容寻址 Blob、经校验的读取、写入器令牌、pin 与 occurrence 保持私有且以 run 为范围；
- LLM、查重、Docker、制品与活跃时间记账保持保守；
- 未知的外部边界对账其原始身份，或保守地暂停；
- 在同一 run 的已验证题包 occurrence 与最终质量报告原子提交之前，READY 仍不可用。

## 后果

该设计更小，可用本地子进程测试，并与实际的阶段 1 运营保持一致。SQLite 是当前投影与审计存储，而不是由重放驱动的调度器。如果未来需求包括远程 worker、没有 CLI 进程的自动定时器、跨宿主恢复或运维工作流搜索，新的 ADR 可以在保留类型化活动与领域账本边界的同时采用托管运行时。

## 契约短语（canonical contract phrases）

架构检查脚本以这些英文短语作为契约锚点：

- one foreground executor per run
- per-run process lock
- fixed pipeline
- no workflow-hosting service
- local fixed loop
- LangChainGo
- internal/application
- internal/agent
- SQLite remains authoritative
