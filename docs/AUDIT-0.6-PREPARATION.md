# 项目整体审查：0.6 前置修复清单

> 状态更新（2026-09-27 北京时间）：本报告记录的是修复前的审查基线。第 1–3、5 项已修复，第 4 项按官方 0.2.8 的接口限制完成安全降级；完整结果与测试证据见 [0.6 修复验收](AUDIT-0.6-REMEDIATION.md)。下文保留原始复现与历史结论，不能当作当前修复状态。

审查基线：`f6049724102592d977305234476e17d6f92aad1f`（main）。产品版本为 `0.5.28`，下一版本计划为 `0.6 / 0.6.0`。

本轮检查了请求路由、认证与代理、账号目录和单账号检测、403 停用、429 隔离、设备收敛、图片上传、流式工具转换、UI Bridge、打包验签和发布脚本，并对照本地官方 Sub2API 0.2.8 的宿主接口。结论为 **5 类已复现问题，其中 1 类 P1、4 类 P2，均尚未修复**。建议解决后再进入 0.6 发布验收。

## 1. P1：流式失败证据不能可靠阻止工具输出

定位：`internal/transport/stream.go:388–424`、`internal/transport/tool_repair.go:98–116,149–156`。

主转发用正文 `type` 覆盖真实 SSE 事件名，并主要根据嵌套 `response.status` 判断完成。错误信封带有 completed 或缺省状态时，外层/内层 error 不能阻止工具完成事件被发给客户端。工具修复路径还会在读取嵌套 response 时丢掉外层通用 error。

本地复现覆盖：

- 真实 `event:error`，正文却含 `type:response.completed`。
- completed 信封存在外层 error、嵌套 response.error，或明确 status_code=403。
- 修复响应含外层 server_error，但嵌套 completed 工具结果仍能合并。

实际输出仍包含 `response.function_call_arguments.done` 和 `response.completed`。这证明客户端会收到可调用工具事件；测试没有执行这些工具。含 403 的用例中，错误虽已被隔离为 `bps_service_rejected`，本轮工具输出仍被释放。账号未来请求的停用逻辑和本轮响应的终止逻辑需要分别保证。

建议：共享终态判定，同时检查真实 SSE 事件名、正文 type、两层 error/status，失败证据优先；失败后清空暂存工具并禁止修复合并及成功上下文缓存。

## 2. P2：JSON 转 SSE 会把失败状态改成成功

定位：`internal/protocol/protocol.go:1327–1329`，入口 `internal/transport/transport.go:1105–1106`。

客户端要求 stream:true，上游返回 HTTP 200 JSON 且 status 为 failed、incomplete 或 cancelled 时，SyntheticStream 无条件把状态写成 completed，并发送 response.completed。三个子用例均复现。下游因此得到与上游相反的终态，即使 error 字段还在。

建议：按原终态生成事件；未完成、失败、取消分别保留语义，不能统一转换成成功。

## 3. P2：缓冲 SSE 解析会丢失失败分类，并接受矛盾终态

定位：`internal/protocol/api.go:55–65,80–83`，调用处 `internal/transport/transport.go:1094–1099`。

普通 HTTP 200 SSE 的 response.failed/status=failed/insufficient_quota 已经由隔离层变成请求级 bps_service_rejected，但 stream:false 的缓冲转换只寻找 completed，最终返回 invalid_upstream_response / ended without response.completed，丢失真正的请求级错误原因。

另两类用例也复现：response.created 或 response.failed 中只要嵌套 status=completed 就能被采纳；先失败、后追加 completed 的同一流也会采纳后者，返回其中工具。

建议：解析器记录并锁定第一个终态，保留失败分类与内容，不再用“正文中曾出现 completed”代替合法完成。此发现不等于已证实宿主发生跨账号重试或错误限流，后者本轮未做生产实测。

## 4. P2：两个配置页并发单账号检测可能检测错账号

定位：`ui/assets/app.js:943–971`、`ui/assets/bridge-v1.js:146–147`。

页面先把目标账号保存到共享配置，再发送无参数 config.test。真实 app.js 的双页面夹具按 save(1) → save(2) → test → test 执行，目标分别是账号 1、2，实际两次检测均发往账号 2，即 `dispatched=[2,2]`。

官方宿主代码交叉核对：`frontend/src/api/admin/plugins.ts:144` 发无正文 POST /test；`backend/internal/service/plugin_manager.go:698–699,786–819` 只分别锁单次 SaveConfig/Test，Test 重新读取共享数据库配置；数据库更新没有配置 revision CAS。该时序在当前宿主可达。

页面 A 会在 app.js:978–980 拒收错误账号的结果，故不会显示冒名成功，但发错账号的请求、额度消耗及可能的 403 停用副作用已经发生。

建议：检测接口原子接收目标账号或与配置版本绑定的快照。仅增加本 iframe 的 busy 锁不能覆盖多页/多人；需要同时考虑现有 0.2.8 Bridge 的兼容路径。

## 5. P2：残留公钥导致默认未签名构建失败

定位：`build.ps1:57–65`、`build.sh:75–76`，校验条件 `tools/verify_package.py:253–254`。

README 允许不传签名参数生成本地调试包，但脚本发现 build/keys/publisher.public 时仍自动传 --public-key。校验器把它解释为“必须有签名”，因此未签名包被拒绝。

隔离目录运行实际 PowerShell 包装器和实际 Python 校验器，仅模拟编译步骤：不存在公钥时退出 0，存在公钥时退出 1，错误为“包中没有 signature.json”。Bash 有同样条件，已做源码核对，当前环境未重跑该 Bash 分支。该问题影响默认调试构建，不说明已发布签名包无效。

建议：仅在明确签名构建时自动传配套验签公钥，继续保留生产签名构建的强制验签；补上有/无签名参数、有/无残留公钥的矩阵测试。

## 既有关注项的结论

- 账号名称展示：完整名称为主、完整 ID 为次；同名账号仍按 ID 路由和保存，相关现有回归通过。
- 单账号检测：普通单页使用时凭据和代理按目标账号绑定；多页并发存在第 4 项缺陷。当前“降智”仍是固定回答规则（明确单一代际 17 才符合规则），不能据此认定真实模型能力。
- 403 自动停用：默认开启、关闭不清历史、按账号 KV 保存及 block_id 精确恢复等常规路径回归通过。本轮未在官方正常生命周期中确认新的状态存储缺陷，但第 1 项说明响应终止仍有遗漏。
- 429 隔离：已有状态码、额度错误及响应头隔离回归通过；第 3 项需要保留隔离后的错误分类。插件无法保证上游不返回 429。
- 设备收敛：实现和开关确实参与 BPS 请求构造，已知设备字段按账号稳定收敛；不会把 IP、TLS、全部未知字段都统一，保留的会话隔离也不是设备指纹。配置开关默认关闭，关闭不能撤销宿主已做的改写。
- 图片路径：本轮复核了原生附件上传、身份和代理继承、缓存范围、大小限制及错误回传，未新增确认缺陷。

设备范围以 `docs/BPS-DEVICE-FINGERPRINT.md` 为准，403 的状态语义以 `docs/BPS-403-ACCOUNT-DISABLE.md` 为准。

## 验证与复现

既有测试重新运行结果：

- `go test ./... -skip TestLive -count=1 -timeout 120s`：通过。
- `go vet ./...`：通过。
- `node --test tools/ui.test.cjs`：261 通过、0 失败。
- Python 测试共 24 项：23 通过，1 项原生 Bash mock 测试在本机跳过。
- 认证、配置、账号状态、设备和降智检测的针对性 Go 回归：通过。

审查专用复现存于忽略目录 build，不修改生产源码；以下命令用于展示仍然存在的问题，协议复现预期返回非零：

~~~powershell
go test "-overlay=build/audit-protocol/overlay.json" ./internal/transport -run "^TestAudit" -count=1 -timeout=30s
node build/audit-ui/reproduce-ui.cjs
python -X utf8 build/audit-ui/reproduce-build.py
~~~

协议复现 7 个测试组失败，日志为 `build/audit-protocol/final-root.log`；UI 和构建复现直接打印实际目标及退出码。以上使用本地模拟数据或内存响应，没有调用生产 BPS 账号。

## 0.6 的修复顺序与维护建议

1. 先修复第 1–3 项，把探测、实时转发、缓冲转换、工具修复的终态规则集中到共享判定，增加失败不能重新完成或释放工具的回归。
2. 再解决单账号检测的跨页面事务边界，补官方宿主契约测试，避免只修 UI 表象。
3. 修默认未签名构建分支，保持正式包独立 Ed25519 验签流程。
4. 分离当前未被生产路径引用的旧 internal/imagerelay 包与历史材料，降低新人误改旧图片链路的概率；先核对外部使用再决定迁移或删除。
5. 若继续做性能优化，优先测量接收/JSON 解析前的并发内存：图片 admission 在接收并解析请求之后，只限制后续图片工作，不是整个进程的 RSS 上限。现阶段作为测量方向，不列已复现的内存故障。

本轮产出是审查报告及本地复现；生产代码、版本、签名包和线上发布均未因本报告发生变更。
