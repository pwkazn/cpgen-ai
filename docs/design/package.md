# 内部题包与导出设计

状态：ADR-0006 下为当前设计

实现说明（2026-09-09）：`cpgen.package/v2` 现通过既有的 `packageprobe` 构建器/读取器提供下述生成题包模型。其布局使用 `statement/statement.md`、`solution/{reference,brute}.{cpp,go}`、`judge/{generator,validator}.{cpp,go}`、固定的 `judge/checker.cpp`、`data/tests.json`、成对的 `tests/*.in` / `tests/*.ans`，以及题包安全报告。V1 保留其历史探测布局。专用 M26 题包账本、当前证据组装、规范 ZIP 导出和原子 READY 事务均已实现，并通过真实 Docker 以及独立 CLI 崩溃/导出/再验证验收。格式校验由同一 run 内已提交的 Quality 证明补充。见 [题包验收证据](../evidence/mvp-package-commit-foundation.md)。

## 1. 原则

题包是由已验证的 run 作用域制品实例确定性组装而成的不可变树。阶段代码不能复制任意工作区文件。所有路径、角色、媒体类型、digest、来源和门禁结果都会被声明和审计。

READY 意味着同一 run 原子地引用一个 VERIFIED 题包实例和最终质量报告。

## 2. 内部目录

~~~text
problem/
  manifest.json
  statement/
    statement.md
  solutions/
    reference.*
  data/
    tests.json
    input/
    output/
  checker/
    checker.*
  reports/
    quality.json
    similarity.json
    verification.json
  provenance/
    artifacts.json
~~~

可选条目由 manifest schema 控制。未声明的文件、符号链接、设备文件、绝对路径、遍历分量、重复规范化路径和大小写折叠冲突都会被拒绝。

## 3. Manifest

规范 manifest 记录：

- schema 版本与题包身份；
- 标题、限制、checker 类型和支持的语言；
- 每个文件路径、角色、媒体类型、大小和 SHA-256 digest；
- 题面、题解、数据、checker、报告和来源引用；
- workflow、请求、配置和策略 digest；
- 工具链和镜像身份；
- 必需的质量门禁及其证据 digest。

规范 JSON 使用稳定字段顺序、UTF-8 规范化、精确整数，且不含任何无实质意义的差异。

## 4. MeteredPackageWriter

题包阶段只接收一个 MeteredPackageWriter。它可以：

- 声明题包角色和规范化相对路径；
- 打开经授权、属于同一 run 和 revision 的已验证源实例；
- 在字节记账下流式写入私有暂存树；
- 完成 manifest；
- 请求结构验证和语义验证；
- 在所有门禁通过后原子发布。

它不能读取任意文件系统路径，也不能直接发布到用户选定的目标位置。

## 5. 构建协议

1. 阶段只加载已验证的当前实例。
2. 一个短事务创建题包身份、STAGING 中的实例、声明和制品字节预留。
3. 在数据库事务之外，写入器构建私有暂存树，通过已验证读取进行复制，对每个输出计算哈希并 fsync。
4. PackageStructuralGate 独立解析该树。
5. 语义门禁校验题面、题解、测试、checker、报告和来源。
6. 创建验证回执和最终质量报告。
7. 在一个短事务中，预留结算，题包实例变为 VERIFIED，run 指向该实例并变为 READY。
8. 发布通过已验证的目标协议原子重命名或复制，并记录结果。

任意时刻崩溃都可安全重放。未验证的暂存内容不能使 run 变为 READY。

## 6. 结构门禁

PackageStructuralGate 验证：

- 规范化路径与精确的 manifest 成员关系；
- 无链接、特殊文件或意外目录；
- 每个文件的 digest 和大小；
- manifest schema 与交叉引用；
- 必需角色和唯一身份；
- 仅题包安全的来源；
- 配置的总字节数和文件数上限。

该门禁将本地暂存和导入的题包都视为不可信。

## 7. 语义门禁

题包门禁汇总：

- 题面完整性和约束一致性；
- 参考题解的编译与执行证据；
- 测试数据有效性和期望输出证据；
- checker 或 SPJ 契约证据；
- Judge 和资源限制证据；
- 差分与质量报告；
- 查重决策或界限豁免；
- 工具链、镜像、workflow、策略和 revision 一致性。

门禁不能因某个阶段结果而被跳过。只有对显式可豁免的规则和精确证据绑定，豁免才会被接受。

## 8. 验证回执

回执包含题包 ID、manifest 与树 digest、门禁版本、工具链身份、验证时间、所有门禁结果 digest 以及最终质量报告身份。它是不可变的且题包安全。

导入的题包先创建 IMPORTED 实例。只有在经过同一套完整门禁序列后它们才变为 VERIFIED；导入绝不信任随包携带的回执而不重新计算。

## 9. READY 事务

数据库强制：

- 题包实例属于该 run；
- 其状态为 VERIFIED；
- 回执和最终质量报告非空且一致；
- relation 与 revision 匹配；
- run 状态与题包指针一起更新。

任何进程本地检查都不能替代这些约束。

## 10. 导出器

导出器只通过已验证读取消费 VERIFIED 的内部题包。Polygon 导出器将规范模型映射为平台文件，记录确定性映射和损失报告，校验导出的树并原子发布。

导出器重试在 Package 阶段内有界。未知的远程上传边界会核对原始提供方身份，或保守暂停；它绝不会以新身份创建第二次发布。

## 11. 修复与复核

题包校验错误路由到最早的归属已编译阶段。机械性暂存错误可在本地重试。内容或策略失败按类型化规则和剩余预算进入 NEEDS_REVIEW 或修订归属阶段。

## 12. 测试

测试覆盖规范 manifest 编码、路径攻击、大小写冲突、未声明文件、符号链接、digest 损坏、确定性构建、每个暂存和发布边界的崩溃、门禁完整性、豁免绑定、导入题包验证、同 run READY 约束、导出器损失报告以及有界重试。
