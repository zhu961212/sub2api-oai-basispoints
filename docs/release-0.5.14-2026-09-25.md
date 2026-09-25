# 0.5.14 图片发送修复

日期：2026-09-25

- 新增自托管临时 HTTPS 图片中转；Base64 消息附件和 function/custom 工具截图继续交给 BPS 的 gpt-6-astra，同账号、同代理，无原生通道回退或第三方图床。
- 修复本地请求校验 HTTP 400 被传输错误帧转成 502 的问题；图片内容错误带安全字段位置，不回显图片和临时下载 token。
- 增加配置页开关、公网 HTTPS 地址、监听与目录设置；仅 GET/HEAD 下载、30 分钟过期、私有磁盘存储、容量与请求预算限制。
- 保留既有模型/账号路由、工具目录及流式恢复逻辑；file_id 和 detail=original 仍按 BPS 适配器能力明确拒绝。

构建目标为 Windows/Linux amd64。验收命令与通用签名步骤见 README，启用条件见 [图片中转说明](IMAGE-RELAY.md)。

验证结果：

- 全量离线 Go 回归通过：go test ./... -skip TestLive -count=1 -timeout 120s。
- Go 静态检查、49 项配置页测试及本地 Chrome 沙箱回归通过。
- 图片测试覆盖真实 HTTPS 下载、转发开关组合、无效输入、会话隔离、满容量复用、清理与关闭，以及 JSON/SSE 错误脱敏。
- Windows/Linux amd64 构建通过；独立验包确认文件哈希与 Ed25519 签名有效，key_id 为 oai-basispoints-v1。

安装包：dist/local.oai-basispoints-0.5.14.s2plugin，13,389,707 字节。

SHA-256：DF6BE8688E7C62B0E0CD9DA4177EC8D924C150FB28FED5C8C4F29B26E1253125。

本次修改属于本地源码与安装包，不代表已替换线上插件或完成真实 BPS 视觉验收。
