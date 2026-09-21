# ADR-0002：run 状态、评审、重试与本地恢复

Status: Accepted（由 ADR-0006 修订）

Date: 2026-08-30

## 背景

阶段 1 需要针对固定本地流水线的持久化重启与人工评审。它不需要分布式所有权或通用工作流引擎。

## 决策

### run 状态

封闭的 run 状态集合为：

- CREATED
- RUNNING
- BLOCKED
- NEEDS_REVIEW
- READY
- FAILED
- CANCELLED

在 Slice 5 原子地绑定同一 run 的已验证题包 occurrence 与最终质量报告之前，READY 不可达。

### 阶段与尝试状态

编译后的阶段具有 PENDING、RUNNING、SUCCEEDED、BLOCKED、NEEDS_REVIEW、FAILED 或 CANCELLED 状态。每次物理执行都是一条只追加的阶段尝试，具有 RUNNING、SUCCEEDED、BLOCKED、NEEDS_REVIEW、FAILED、CANCELLED 或 INTERRUPTED 状态。

异常的进程退出可能使 run 与当前尝试被记录为 RUNNING。下一次人工 resume 命令会获取每 run 进程锁，对未完成的沙箱工作进行对账，记录被中断的尝试，并根据其领域证据启动或重放当前阶段。

### 重试与阻塞后恢复

重试是有界的，并由当前阶段策略负责。物理重试获得新的尝试序号与调用记录，同时保留稳定的逻辑幂等键。未知的外部发送边界必须对账原始身份，或保守地结算为类型化的暂停或失败。

BLOCKED 保存当前阶段输入摘要、依赖身份、策略摘要、错误证据与重试等待时间。人工 resume 会为该同一阶段创建一次全新尝试。其首个被授权的操作通过普通计量端口与当前策略重新校验依赖；过期的能力数据本身不能恢复工作。

### ReviewDecision

ReviewDecision 的种类为 REVISE、RETRY、WAIVE 与 REJECT。其生命周期为 PENDING、APPLIED、REJECTED 或 STALE。

评审命令创建不可变的 PENDING 决策。它们不直接修改生成内容，也不推进 run。人工 resume 在校验 run 版本、修订号、证据、策略与预算绑定之后，于一个短事务中恰好应用一个匹配的决策。

### 取消

取消命令插入一条幂等的控制请求。前台执行器轮询它，取消根上下文，停止授权新工作，并结算已经授权的效果。

在每一个不受信任的沙箱目标都被证明已停止之前，run 不能变为 CANCELLED。如果没有执行器持有进程锁，取消命令可以获取它，对精确持久化的沙箱资源进行对账，并提交终态。清理证据仍可在取消之后结算，但任何普通阶段工作都不得启动。

## 后果

- 七个 run 状态足以用于 CLI 呈现与持久化。
- 重启行为针对当前阶段且领域特定。
- 评审不可变、可审计，且仅由协调器应用。
- 取消保持响应性，同时不允许一个 run 出现两个执行器。
- Docker 停止安全性由 SandboxExecution 状态表示，而不是额外的 run 模式。

## 被取代的设计

<!-- Superseded design: begin -->
更早的设计要求 NORMAL、探测中、静默中三种模式，以及执行租约、栅栏纪元、观察票据和通用恢复意图。ADR-0006 用一把由操作系统支撑的 run 锁、期望版本的数据库写入、全新的同阶段尝试和领域特定的对账取代了这些机制。
<!-- Superseded design: end -->
