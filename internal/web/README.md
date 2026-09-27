# 本地工作台

后端为 Go 标准库与 Gin；TypeScript 页面构建产物通过 `embed` 随二进制发布。

```powershell
go run ./cmd/cpgen --config config.yaml serve --listen 127.0.0.1:8080 --capacity 1
```

打开终端输出的本机会话链接。仅支持回环地址，默认一个生成槽位；容量范围为 1–64。Ctrl+C / SIGTERM 停止接纳新任务，并等待已接纳任务和清理完成。服务重启不会自动继续历史任务。

页面包含任务列表、新建、详情、待评审和只读配置。评审只支持重试（增加预算）或拒绝；保存决定后仍需显式继续执行。取消请求持久化后由执行器或独立清理任务完成，不把“已接受请求”直接显示为“已取消”。

详情通过一次 SQLite 事务读取预算、阶段、待处理决定与最近事件。题面、源码和报告按需读取，验证 occurrence 的归属、当前阶段、来源和内容摘要；模型私有响应仅用于恢复已校验的领域内容，不直接送入浏览器。题面预览标为草稿；READY 题面来自已验证题包。单次内容预览限制为 2 MiB。执行器占有运行锁时，内容重建返回冲突，可稍后重试。

创建身份绑定规范化请求；恢复身份及任务版本写入 SQLite，重复请求不会再次启动。已接纳但因进程退出未执行的恢复请求也保留身份，需用户重新读取状态并发起新的显式恢复。取消和评审复用领域持久化身份。

修改前端后执行：

```powershell
cd internal/web/ui
npm ci
npm run build
```

`ui/app.ts` 是页面源码，`static/app.css` 和 `static/index.html` 为样式与入口，`static/app.js` 为提交的构建产物。运行 Go 程序不需要 Node.js。Marked、DOMPurify 和 KaTeX 随包离线发布；公式输出 MathML，禁止原始 HTML 与自动加载外部图片。

集成测试使用本地 Fake 工作流和测试 provider，不消耗线上模型预算。
