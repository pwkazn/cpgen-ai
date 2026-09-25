# 内部题包与导出设计

状态：ADR-0006 下为当前设计

范围说明：当前产品提供本地规范 ZIP 导出与已有题包的只读证据重建。下文关于通用导入后执行、Polygon 格式转换/损失报告和远程上传对账属于后续设计，不是现有 CLI 能力；第 12 节含这些扩展的测试目标，不代表它们已完成验收。

当前实现：MVP 工作流 V3 使用 `cpgen.package/v3`，历史 MVP V1/V2 run 保留 `cpgen.package/v2`；更早的探测格式 v1 保留自己的布局。这些版本由既有 `packageprobe` 构建器/读取器按持久化身份处理，不自动升级旧包。生成题包包含 `statement/statement.md`、`solution/{reference,brute}.{cpp,go}`、`judge/{generator,validator}.{cpp,go}`、固定的 `judge/checker.cpp`、`data/tests.json`、成对的 `tests/*.in` / `tests/*.ans` 以及报告。V3 的 `statement/samples.json` 绑定执行定稿，题面与样例输出从只读证据链重建；见[执行样例设计](executed-samples.md)与 [V3 验收](../evidence/ready-stability-2026-09-22.md)。

专用 M26 题包账本、规范 ZIP 导出和原子 READY 事务的初始实现记录见 [2026-09-09 题包验收](../evidence/mvp-package-commit-foundation.md)。格式校验由同一 run 内已提交的 Quality 证明补充。

## 1. 原则

题包是由已验证的 run 作用域制品实例确定性组装而成的不可变树。阶段代码不能复制任意工作区文件。所有路径、角色、媒体类型、digest、来源和门禁结果都会被声明和审计。

READY 意味着同一 run 原子地引用一个 VERIFIED 题包实例和最终质量报告。

## 2. 内部目录

当前普通 C++ 生成 ZIP 使用以下布局。完整路径与角色以对应版本 manifest、`internal/application/package_reader.go` 组装器及 `internal/packageprobe` 校验器为准；测试 ID 按来源与序号生成。

~~~text
problem.zip（归档根目录）
  manifest.json
  statement/
    statement.md
    samples.json
  solution/
    editorial.md
    reference.cpp
    brute.cpp
  judge/
    generator.cpp
    validator.cpp
    checker.cpp
  data/
    tests.json
  tests/
    sample-001.in
    sample-001.ans
    generated-001.in
    generated-001.ans
    ...
  reports/
    similarity.json
    prepackage-quality.json
    provenance.json
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

当前 `run export` 只通过已验证读取消费 VERIFIED 题包，重建预期 ZIP 并验证已存归档。Polygon 导出器尚未实现；未来若接入，需要将规范模型映射为平台文件，记录确定性映射和损失报告，并校验导出的树。

当前导出只写本地文件，不执行远程上传。远程发布重试与未知上传边界的对账属于后续设计，不能复用“本地导出通过”作为验收证明。

## 11. 修复与复核

题包校验错误路由到最早的归属已编译阶段。机械性暂存错误可在本地重试。内容或策略失败按类型化规则和剩余预算进入 NEEDS_REVIEW 或修订归属阶段。

## 12. 测试

测试覆盖规范 manifest 编码、路径攻击、大小写冲突、未声明文件、符号链接、digest 损坏、确定性构建、每个暂存和发布边界的崩溃、门禁完整性、豁免绑定、导入题包验证、同 run READY 约束、导出器损失报告以及有界重试。
