# 0.5.23：修复工具截图后续回合的图片协议

## 问题与证据

用户反馈发送图片后，模型已经开始修改或调用工具，随后返回通用 HTTP 422。旧实现会将用户消息图片和 function/custom 工具结果截图一并上传并替换为 file_id。

2026-09-26 由 [官方 Excel Manifest](https://bps.openai.com/basispoints/api/office/manifest.xml) 定位到 [当前前端脚本](https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/assets/x-square-DUrhLSGN.js)：Nme 为用户消息构造 file_id，lge / Eqr 为工具结果构造 image_url data URL，并使用 nullish 默认值处理 detail。原实现没有保留这两类内容的协议差异。

本版修复该可确认的兼容性差异。截图只显示通用 422 与请求 ID，没有服务器具体字段校验结果，不能据此认定这就是实际生产请求的唯一失败原因，也不能宣称线上已修复。

## 修复行为

- 用户消息内的 Base64 图片继续通过原生 multipart/form-data 的 file 字段上传原始字节，以返回的 openai_file_id 写入 file_id。账号、认证和代理均沿用宿主当前调度结果。
- function_call_output / custom_tool_call_output 的 output 数组截图保留原始 image_url data URL，不再自动上传并替换为用户附件 file_id。
- 缺失或 null 的 detail 统一补为 auto，保留显式 low/high；不压缩、不缩放，不把 original 静默降级。
- 工具截图仍完整校验 Base64 全部字节、MIME、文件格式与尺寸，并计入单张、数量、总解码量及请求并发预算；仅带工具截图的请求也不能绕过准入。
- 混合请求先校验全部内嵌图片，再上传需要上传的用户图片。任一图片无效或上传失败时，不发送后续 Responses 请求，不部分改写图片引用。
- 保留 0.5.22 的 BPS 429 隔离、模型选择、账号白名单、会话隔离、代理及错误脱敏行为。

## 升级步骤

1. 在插件管理页安装并重新加载 0.5.23，确认配置页显示运行版本 0.5.23。
2. 保留已选模型、账号和灰度设置，重新发起带图片的请求，继续到修改或截图工具返回之后。无需配置图床、公网地址或额外图片开关。
3. 若仍出现 422，保留发生阶段、请求 ID 和脱敏错误信息，检查实际服务器拒绝字段；不要公开 Token、Base64 图片、原始文件 ID 或敏感工具参数。

安装包为 dist/local.oai-basispoints-0.5.23.s2plugin；源码导出目标为 dist/sub2api-oai-basispoints-0.5.23-github-source.zip。重新构建后应重新验签并计算哈希，不能套用历史包哈希。

## 验证范围

本地检查已通过：

- go test ./cmd/... ./internal/... ./tools/... -skip TestLive -count=1 -timeout 120s，覆盖全部生产包目录。
- go vet ./cmd/... ./internal/... ./tools/...。
- node --test tools/ui.test.cjs：122 项通过。
- 新增双轮回归覆盖 function/custom × SSE/JSON × detail 缺失/null，共 8 种组合；仅工具截图的请求确认零附件上传。
- 以严格遵循官方前端形状的本地 fixture 对比：旧 HEAD 代码通过 Go overlay 运行时在工具后的第二轮返回 422，当前代码通过相同场景。该结果是本地协议兼容性复现，不是线上 BPS 测试。

图片回归同时覆盖用户图片 multipart 原始字节及 file_id、显式 low/high、混合图片路径、完整字节校验、大小/数量/预算、代理、取消、失败和脱敏。工具截图的 Base64 错误脱敏测试通过；新增 72 项格式边界回归通过，覆盖 function/custom、两种转换开关的四种组合、空白或大小写异常的类型与图片 URL、非法 Base64，确认全部在上传或转发前返回 400，释放准入预算且不回显图片内容。

未用真实 OAuth 账号完成此次图片识别与工具后续回合的上游验收，未向运行中的宿主部署。本地回归与官方前端静态证据不等同于真实识图或生产问题复现。0.5.22 的历史六模型和一键检测实测仅适用于当次记录，不替代本次图片验收。

## 签名安装包

产物：dist/local.oai-basispoints-0.5.23.s2plugin，13,136,762 字节，包含 Windows/Linux amd64 运行时。沿用发布身份 oai-basispoints-v1；配套公钥独立 Ed25519 验签和全部成员哈希检查通过，配套 .sha256 文件已生成。

SHA-256：761cf737e781c3336691ad3e10480544c1cef07ff67bddba54ff7dedbbc2f88d
