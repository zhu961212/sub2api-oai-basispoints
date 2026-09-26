# 0.5.25：临时上游故障恢复与图片、超时诊断

用户报告运行 0.5.24 时，生成 HTML 出现 stream disconnected before completion: Upstream service temporarily unavailable，同一会话发送多张图片后出现 Upstream request failed。

## 已确认的原因范围

本地宿主代码会将 HTTP 500/502/503/504 映射为 Upstream service temporarily unavailable。HTTP 422 等其他错误、部分插件传输错误会被包装为 Upstream request failed；BPS 429 也可能通过 PLUGIN_RATE_LIMITED 落到通用错误兜底。两条界面文案不能确定原始状态码，也不能证明 HTML 内容有问题。

插件默认 timeout_seconds=300，表示单次上游请求的总时长，持续收到输出或 15 秒心跳都不会延长它。0.5.24 把流读取超时和连接异常合并为 upstream_read，非流式路径还把读取失败误标为 upstream_response_too_large。

图片限制作用于本次请求的完整历史，包括用户图片、function/custom 工具截图和重复出现的图片。上限为 20 个内嵌图片块、32 MiB 解码后总量；单张仍为 20 MiB。连续回合会重新携带历史，所以本轮只新增一张图也可能超限。该限制触发明确的本地 400 invalid_image，不足以证明就是截图中上游错误的原因。

## 本次修复

- 已准备的 BPS Responses 请求在 HTTP 500/502/503/504 时最多再试两次，默认退避 250/500 毫秒，复用相同账号、代理、正文、会话标识和已上传附件。
- 仅在尚未向下游转发响应时恢复。429、其他 4xx、网络异常及 HTTP 200 后的流失败不触发重放；上游 X-Should-Retry=false 时也不重试。Retry-After 等待最多 5 秒，要求更久时直接保留原失败响应。不会把持续故障伪装为成功。
- HTTP 重试与初次响应读取共用配置的 deadline。附件准备仍在该 Responses 阶段之前，既有工具纠正请求仍遵循其原有单独请求预算。上游处理或计费是否幂等不由插件保证。
- 超时提示检查 timeout_seconds，连接截断与其他连接错误使用固定脱敏信息；已输出的流仍以 response.failed、单一 [DONE] 和结束帧收尾，未完成工具不会执行。
- 非流式读取失败不再误报体积超限。图片上游错误保留脱敏的 HTTP 状态、type/code/param，不转发验证器回显的 input；图片累计超限提示当前已遇到的数量或解码大小。

保留原有图片内容、尺寸、detail 语义和资源限制。未修改宿主的通用错误映射，因此某些客户端仍可能显示通用文案，应结合宿主 Ops 记录里的原始状态和脱敏诊断定位。

## 验证与部署边界

本地模拟上游已复现旧版遇到短暂 5xx 立即失败：500/502/503/504 分别覆盖文本和图片请求，旧实现 8 个子用例全部失败。修复后相同用例在第三次成功响应时完成，图片只上传一次。

2026-09-26 验证结果：

- go test ./cmd/... ./internal/... ./tools/... -skip TestLive -count=1 -timeout 120s：生产源码包完整回归通过。
- go vet ./cmd/... ./internal/... ./tools/...：通过。
- node --check ui/assets/app.js、node --check ui/assets/bridge-v1.js：通过。
- node --test tools/ui.test.cjs：149 项通过。
- 新增回归覆盖重试耗尽、429/4xx 不重试、Retry-After、明确禁止重试、截止时间、取消时释放资源、完整请求与凭据保持、网络失败及部分输出不重放、超时/截断/体积区分、图片错误脱敏和历史累计限额。
- git diff --check：通过。
- Windows/Linux amd64 均已重新编译，独立验包器验证成员哈希和清单通过。

本次未取得生产请求 ID 或完整上游错误日志，未用真实 OAuth 图片会话复现截图，也未部署运行中的宿主。不能据此宣称所有上游失败均已消失。

安装并重新加载 0.5.25 后，先确认实际运行版本，再复测长 HTML 和多图片会话。若日志明确显示配置超时，可在现有插件配置中将 timeout_seconds 调为 600 或 900，并同步检查宿主、反向代理的超时；已有配置不会自动被覆盖。超出历史图片预算的任务应分会话或减少历史图片。若仍失败，按 request_id 查看原始 HTTP 状态、插件错误码、消息和图片字段路径，无需提供 Token 或图片 Base64。

## 签名安装包

产物：dist/local.oai-basispoints-0.5.25.s2plugin，13,134,652 字节，包含 Windows/Linux amd64 运行时。

SHA-256：f4dd48ad4f810c6eb5786a8cba86fa26c012c278b04326a65329e8184d10ef9c。

签名身份沿用 oai-basispoints-v1；原发布公钥的独立 Ed25519 验签和成员哈希校验均通过。已经信任该公钥的部署无需更换信任配置。附件提供签名安装包、SHA-256、公钥、源码 ZIP 和源码校验文件；私钥不进入 Git 仓库、源码包或 Release 附件。

GitHub 发布与线上宿主部署分开进行。安装后须在宿主重新加载并确认运行版本为 0.5.25，再完成真实长 HTML 和多图片会话验收。
