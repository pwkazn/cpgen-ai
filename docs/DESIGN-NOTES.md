# 设计说明

两个问题塑造了 CPGen 架构的大部分。两者源自同一个根因：**生成器不可靠，而其错误的
后果代价高昂且难以撤销。** 本说明解释系统如何把这些错误限制在有界范围内，并使其
恢复代价低廉。

---

## 1. 未知的发送边界：为什么“那个请求发出去了吗？”才是难题

### 问题

模型调用不是纯函数。它花费金钱，会在远程服务上改变状态，并且可能在**调用方无法判断
工作是否已经发生**的位置失败。

考虑一个超时的 POST：

- 提供方从未收到它 → 重试是免费且正确的。
- 提供方收到了它、生成了响应，而*响应*丢失了 → 重试要付两次钱，而系统已经计费过一次。
- TCP 接受了字节，但提供方在持久化任何内容之前崩溃 → 重试是否被幂等处理，取决于供应商。

从进程内部看，这三种情况是**无法区分的**。朴素的实现会选定一种解释并到处套用。两种
选择都以某种要紧的方式出错：

| 朴素选择 | 失败模式 |
|---|---|
| 总是重试 | 响应丢失时重复计费；预算核算向下偏离现实 |
| 从不重试 | 瞬时网络故障导致永久停滞；一次小抖动就需要人工介入 |

CPGen 的答案是**不去猜测**，而是把这个边界变成显式的、有类型的、一等值并加以持久化。

### 机制

每一次物理外部调用都携带一个 `PhysicalBoundary`
（`internal/domain/budget.go`）。区分三种情况：

~~~text
CONFIRMED_NO_SEND   we know, from a specific typed error, the request never left
COMPLETED           we have a response, so the request certainly arrived
UNKNOWN             anything else — the honest answer
~~~

关键的设计决策是**回退的默认方向**。在
`internal/agent/openai.go` 中，传输错误路径写道：

~~~go
if errors.As(err, &adapterErr) {
    // An injected transport may return a typed policy/transport error
    // carrying the explicit no-send boundary. Preserve that boundary;
    // every other transport error is conservatively treated as sent.
    return nil, 0, "", 0, !adapterErr.ConfirmedNoSend, adapterErr
}
~~~

`!adapterErr.ConfirmedNoSend` 的意思是：**只有显式的、有类型的“未发送”断言才被信任。**
拨号失败、TLS 错误、意外的 EOF、被取消的 context，以及任何无法识别的情况，都会落入
`UNKNOWN`。系统从不根据泛化的症状推断“大概没有发送”——它要求正向证据。

这是刻意的不对称。一个错误的 `UNKNOWN` 代价是一次保守的预算扣费，可能还有一次人工
查看。一个错误的 `CONFIRMED_NO_SEND` 代价是无声的双重支出，更糟的是，一条被污染的
审计轨迹。前者只是麻烦；后者会摧毁整个系统赖以存在的性质。

### 成本核算遵循同样的不对称

一旦边界为 `UNKNOWN`，该 run 仍可能被扣费。由于提供方报告的用量按定义不可用，CPGen
收取的是一个**有文档记录的上界**而非零（`internal/agent/openai.go`）：

~~~go
// Provider usage is optional. Bytes are a conservative token upper bound
// for accounting purposes (one UTF-8 byte cannot encode more than one
// token), and output is charged at the configured reservation ceiling.
input := int64(requestBytes)
if input < 0 {
    input = 0
}
return port.Usage{InputTokens: input, OutputTokens: maxOutput}, "conservative_upper_bound_v1"
~~~

`usageSource` 字符串（`conservative_upper_bound_v1`）与数字一起被持久化。这对可审计性
很重要：后续的读者可以区分“我们知道提供方收取了 X”和“我们假设至多 Y”，而无需重建
原始事故。

预算是**在不可逆工作之前预留**，之后单调结算。预留正是保守回退安全的原因——钱已经
预先留出，因此 `UNKNOWN` 结算只能*释放*余量，绝不会超支。

### 恢复是分级的，而非泛化的

崩溃恢复不套用单一的 retry 策略。`ARCHITECTURE.md` §10 定义了由账本所述内容驱动的
逐阶段规则：

| 账本状态 | 恢复动作 |
|---|---|
| 未授权任何外部效应 | 自由重跑该阶段 |
| 效应已发送，稳定身份已记录 | 重放或对账*同一个*提供方身份 |
| 边界未知 | 保守结算；暂停以进行有类型的评审或失败 |
| Blob 已发布，尚未附加 | 校验字节，然后附加或释放 writer token |
| 沙箱资源已持久化 | *仅*对账那些确切的资源身份 |
| 结果已提交 | 正常推进到下一阶段 |

同样的推理延伸到 Docker。`internal/adapter/sandbox/docker/runner.go`
在调用 `create` **之前**持久化一个 `SandboxExecution` 及其完整的计划资源集，因此
create 中途崩溃会留下关于可能存在的资源的持久记录。之后的 reconciler 可以检查、
stop、kill、wait 和 remove *仅*那些具名资源。它不能启动新工作或发布制品。

### 这带来什么

- 崩溃绝不会无声地重复扣费或无声地丢失工作。
- 恢复的可预测性来自状态，而不是关于错误字符串的启发式。
- 审计轨迹区分实测事实与保守假设。
- 系统朝人工评审方向*失败关闭*，而不是朝未经审计的支出方向*失败开放*。

### 坦诚说明的取舍

保守结算可能对一次实际上未能发送的 run 超额收费，`UNKNOWN` 结果也可能把一次人工会
判定为“显然没问题，重试即可”的 run 推入评审。这种摩擦是刻意接受的。替代方案——一个
在“钱是否花了”上通常正确的系统——不可审计，而可审计性正是产品本身。

---

## 2. 为什么 LLM 绝不能评判自己的输出

### 问题

“生成一道算法竞赛题”的显而易见架构是一条模型调用流水线：要一份题面、要一份题解、
要测试数据、让模型检查题解是否能通过数据。演示效果很好。

它也是错的，原因在于结构，而非模型质量：**产生看似合理的错误答案的同一种能力，正是
被要求去发现该错误的能力。** 一个写出微妙错误参考题解的模型，极大概率也会宣称该题解
正确——它生成了一个连贯的故事，并会继续这个故事。加上“请再检查一遍”不会改变这一点，
因为错误不是粗心；而是缺少可执行的真值基准。

对于题目存档，这种失败严重且延迟显现。错误的参考题解不会明显破坏任何东西。它会无声
地产生错误的 `.ans` 文件，进而误判未来的每一次提交——包括正确的提交。损害在数月之后、
在别人的比赛里才浮现。

### 机制

CPGen 的规则陈述起来很简单，却在各处承重：

> **生成模型提出候选。确定性 validator 裁决。**

没有任何阶段接受模型关于正确性的断言。相反，流水线会编译并执行：

~~~text
Request
  -> Idea        -> Statement   -> Similarity
  -> Solution    -> Data        -> Judge      -> Quality
  -> Package gates -> READY
~~~

只有当 run 原子性地引用一个已验证的题包 occurrence *且*引用最终质量报告时，才可能到达
`READY`。不存在从部分验证状态到达 `READY` 的代码路径，模型没有投票权。

### 区分“程序行为异常”与“答案错误”

最微妙的一点是，testlib 风格的工具**通过退出码报告业务结果**，而非零退出码按惯例意味着
“错误”。validator 用非零退出表示“此输入无效”。如果沙箱把任何非零退出都映射为泛化失败，
流水线就无法区分这些情况：

- 选手的题解错误 → **WA**，一个合法判决
- validator 拒绝了格式错误的输入 → **INVALID**，一个合法判决
- checker 崩溃 → **基础设施故障**，根本不是判决

ADR-0003 通过分层结果并让*角色*决定解释来解决这一点，而不是仅凭退出码：

~~~text
CompileOutcome   = OK | CE | INFRA_ERROR
ProcessOutcome   = EXITED | SIGNALED | TLE | MLE | OLE | INFRA_ERROR

ValidatorOutcome = VALID | INVALID | VALIDATOR_ERROR
CheckerOutcome   = AC | WA | PE | CHECKER_ERROR
SolutionVerdict  = OK | RE | TLE | MLE | OLE
~~~

配合固定的、带版本的退出码映射（`testlib_v1`，在工具链清单中以摘要钉定），且
`INFRA_ERROR` **优先于**角色适配器。沙箱本身从不产生 RE、WA 或 PE——它不知道这些含义。
它报告进程做了什么；由评测决定其含义。

真正要紧的后果是：**checker 崩溃不能被洗白成内容失败判决。** 它在到达角色适配器之前
就短路为 `BLOCKED` 或 `FAILED`，且不能触发内容“修复”——因为一个可信工具崩溃就重新
生成一道完全正确的题目，会比停下来糟糕得多。

### 模型不允许为自己撰写证据

这是该设计最锋利的边缘，因此它在读路径上被强制执行，而不是在写路径上被信任。

当后续阶段消费先前结果时，它会重新验证已提交的链条，而不是相信记录下来的摘要。正如
`ARCHITECTURE.md` 所述，题解检查的已验证读路径：

> 验证当前成功的阶段、原始源 bundle、冻结的 Docker
> 请求身份、结果回执、保留的流/程序以及已完成的
> 清理，然后从已验证的 stdout 字节重新计算 token 比较。它拒绝
> 未提交的输出、已更改的输入/策略、缺失的源或 stdout 以及无关
> 的额外制品。**模型无法提供这一执行证据。**

换句话说，期望输出与实际输出之间的比较，不是从模型可能影响过的报告里读取的。它是
**从由可信 runner 捕获、并在写入时内容寻址的 stdout 字节重新计算**出来的。如果字节
缺失或其摘要不匹配，读取就失败关闭。

存储强化了这一点。每个制品在写入前先声明，在哈希的同时流式写入私有临时文件，然后
原子性地提升到其规范 SHA-256 身份。一个*occurrence* 把这些字节绑定到产生它的 run、
阶段尝试、角色和修订——因此“这份测试数据所验证的参考题解”是一个可核查的事实，而不是
提示词中的声称。

### 这带来什么

- 幻觉出来的参考题解产生的是**被拒绝的 run**，而不是损坏的 `.ans`
  文件。模型错误的爆炸半径是一次 run，而不是一个存档。
- 基础设施故障与内容故障被路由到不同路径，因此不稳定的
  checker 绝不会导致一道好题被重新生成。
- 每个验收决策都可追溯到可执行制品——一次编译器退出、一个
  进程结果、一次 token 比较、一个摘要——第三方可以重新运行。
- 导出的 ZIP 可以由一次全新的 CLI 调用独立重新验证，
  因为没有任何关键内容只存在于模型的 context 中。

### 坦诚说明的取舍

这比让模型自查更慢、更贵，而且需要真实的 Docker Engine、钉定的工具链和可用的 testlib
环境。它还意味着系统*无法*生成正确性无法通过执行验证的题目——没有基于证明的评分，没有
部分分，没有主观的题面质量。这些限制是被接受的：一个无法证明其输出正确的生成器，没有
资格写入评测的测试数据。

---

## 关于被放弃方案的说明

ADR-0006 最初在应用层放置了一个 LangGraph 风格的调度器。它被移除了。两件事出了问题，
且都很有教益：

1. **它重复了事实来源。** SQLite 已经持久化阶段状态；
   图库持久化自己的检查点。两个存储可能不一致，而
   对账它们严格地比只有一个更难。
2. **它把提供方库泄漏进领域契约。** 流定义开始
   用领域类型表达调度关注点，而这正是端口
   层存在所要保护的边界。

替代品是**编译进二进制的固定循环**。阶段顺序位于
`internal/workflow/definition.go`，运行时不可编辑；数据库行
选择一个兼容的定义，但从不定义图边。该类型自己的文档
注释直接陈述了意图：

~~~go
// Definition is a compiled compatibility contract, not a configurable graph.
// Persisted names remain unchanged when application constructors evolve.
type Definition struct {
    revision        string
    stages          []domain.StageName
    generation      bool
    preserveAttempt bool
}
~~~

LangChainGo 仍然局限于 `internal/agent`。

普遍的教训，也是这被记录而非悄悄回退的原因：
**不被需要的灵活性是一种负债。** 对于一个单主机、每次 run 单进程的工具，
运行时可编辑的图从来不是产品需求。抽象成本立即支付，而收益从未收回。

完整记述见
[`docs/evidence/architecture-follow-up-2026-09-14.md`](evidence/architecture-follow-up-2026-09-14.md)
和 [ADR-0006](adr/0006-lightweight-local-workflow.md)。
