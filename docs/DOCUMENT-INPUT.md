# PDF / Word 输入修复

日期：2026-09-27。本文对应 0.6.6 源码与发行构建；发布不表示已更新正在运行的线上插件。

## 已修复的失败路径

- 原先 input_file 与音视频一起被能力校验拒绝，PDF/Word 附件无法进入上传流程。现在 PDF、DOC、DOCX 按当前 BPS 官方前端的普通文件路径上传，再以 input_file/file_id 提交。
- PDF/Word 渲染工具返回 detail=original 时，原先会在本地返回 400。现在仅把该提示映射为 high；原始图像字节、尺寸、URL、ID 和相邻文本保持不变。不会在插件内缩放或压缩图片。
- 关闭工具改写或响应转换时，也执行文件上传、输入验证和原始清晰度提示正规化。保留既有账号、代理、工具历史和返回格式。

Upstream request failed 是宿主的通用文案，单凭它无法确定唯一原因。插件本地无效文件仍返回带字段路径的明确 400；真实上游拒绝保持其状态。宿主可能将部分 422 等错误显示为通用提示，本次不改变宿主错误映射或伪造成功响应。

## 支持的输入

- 消息 content，以及 function_call_output / custom_tool_call_output 的 output 数组中的 input_file。
- file_data 为 Base64 data URL：application/pdf、application/msword、application/vnd.openxmlformats-officedocument.wordprocessingml.document。可省略 filename，自动按类型生成名称。
- file_data 为原始 Base64：必须提供以 .pdf、.doc 或 .docx 结尾的 filename；名称不能包含路径分隔符或控制字符。
- 已存在的原生 file_id 不重复上传；它必须可由当前选中的 BPS 账号访问。不会把其他服务/账号的文件 ID 自动迁移到当前账号。
- 不代理下载 file_url；请由客户端读取文件并发送 file_data，或通过客户端工具提取内容。

上传使用与 Responses 相同的当前账号、认证头、代理和同源附件接口；Multipart 只包含 file 字段，保留原始文件内容和名称。文件字节流式解码，不写入代理本地磁盘。文件的缓存摘要包含内容、类型、名称、可信会话和认证身份；复用现有最多 512 项、最长 30 分钟的有界元数据缓存。

## 资源和失败边界

单文件最多 20 MiB，单请求最多 20 次内嵌文件出现。文件与图片各自计数，但解码后合计最多 32 MiB，展开后的对话历史和重复附件都计入。混合请求在任何上传前先共同校验；同时共用 64 MiB 请求准入上限、32 个并发请求和 512 MiB 估算预算；这不是进程内存的硬上限。

上传前校验全部附件的 Base64、文件名称、MIME/扩展名及 PDF/OLE/ZIP 文件头。文件头校验不是完整 PDF 或 Office 格式解析，也不保证加密、损坏或超模型上下文的文档可被读取。全部文件上传成功才改写文件引用；失败时不发送后续 Responses 请求，已上传文件可能仍保留在上游。混合附件不会因后面的文件校验失败而提前上传图片。

不跟随上传重定向；上传失败提示不回显文件字节、凭据或上游错误正文。已有原生 file_id 的请求即使无需上传，也在错误响应路径中脱敏文件 ID。文件资料缓存期限不代表提供方的文件保留期限。

## Word 读写工具

Word 的创建和修改由客户端实际声明的 shell、Python 或文档工具执行。本次回归覆盖 pdfplumber / python-docx 脚本、多行文本、引号、Windows Unicode 路径、普通函数和 custom 调用的参数还原，及 JSON/SSE/历史回放。代理不内置 Word，也不会把仅出现在执行器说明中的嵌套 helper 当作可直接调用的工具。

## 验证范围

- 文件内容预校验、原生 ID 保留、准备阶段不修改原始历史、非法输入与错误脱敏。
- PDF 上传 72 种本地组合：3 种容器、3 种输入表示、改写/转换/流式各两种开关。
- original 截图 72 种本地组合：3 种容器、inline/HTTPS/file_id、改写/转换/流式各两种开关。
- 文件上传包测试覆盖 PDF/DOC/DOCX multipart 原始字节、缓存、预算、取消和失败路径。
- 完整 Go 回归和 go vet；官方协议来源见 internal/attachments/README.md。

本地 HTTP/SSE 测试使用合成数据；未使用真实 OAuth 账号进行 PDF/Word 内容识别验收，也未部署生产服务器。
