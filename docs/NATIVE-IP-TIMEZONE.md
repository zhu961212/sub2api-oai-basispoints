# 请求时区跟随出口 IP

本功能从 0.6.7 提供，发布不表示已经部署或通过真实账号验收。更新插件后，在“请求环境”区域打开“请求时区跟随出口 IP”并保存。开关默认关闭，与自动降智检测开关独立。配置沿用旧字段名 `native_timezone_by_ip`（默认 `false`），现在共同控制原生 Codex 与 BPS 两类请求，旧配置无需迁移。

## 请求行为

- 两个开关相互独立：“请求时区跟随出口 IP”只控制时间上下文；“自动检测与切换”控制定时检测及业务路由切换。降智检测始终只请求原生 Codex，不检测 BPS。即使某账号的业务已切到 BPS，仍通过原生 Codex 检测恢复情况；检测请求本身遵循时区开关。
- 作用于经过本插件的原生 Codex 业务、BPS 业务和原生降智检测。原生请求匹配 HTTPS chatgpt.com 的 /backend-api/codex/responses 和 /backend-api/codex/responses/compact；BPS 请求按照已选 BPS 路由处理，包括用户配置的自定义 `responses_url`。非 Codex 透传与绕过插件的请求不在范围内。
- 使用实际转发账号对应的同一代理客户端向 https://ipapi.co/timezone/ 发送独立 GET。BPS 按最终转发身份解析的代理查询；未配置账号代理时使用插件的直接出口。查询不携带账号 Token、账号头、业务正文或 Cookie，不跟随重定向。
- BPS 在协议准备前更新模型请求的时间上下文，因此后续重试和工具修复继续继承同一时区。文件和图片上传本身不注入时间上下文。
- 查询返回合法 IANA 时区后，更新最近一个可识别的完整独立 user environment_context 中的 timezone，并按同一时区计算当前 current_date。缺少该上下文时加入独立环境消息。
- 时区处理保留指令、普通用户文本、工具输出、图片和原有内容注解。已有 content_item_kinds 注解时，仅修改 environments.environment_context 类型；原生新增环境消息带独立注解，BPS 新增环境消息不添加原生专用注解，后续仍遵循原有 BPS 协议转换规则。其他消息的内容索引不变，日期不可用标记会替换为有效当地日期。
- 关闭时不开始查询。查询失败、超时、限额、返回无效时区，或请求编码/格式不支持时，保留原请求。开关关闭前已经开始的查询最多继续到其 2 秒上限。

## 查询与自动检测

- 查询最长 2 秒；同账号及代理配置共享缓存和进行中的查询。成功缓存 6 小时，失败缓存 5 分钟。最多 1024 条缓存、8 个并发查询，达到并发上限时直接放行原请求。
- 缓存只保存时区，不保存实际 IP 或代理凭证；缓存键使用账号 ID 和代理配置 SHA-256。内嵌时区数据库，日期计算遵循 IANA 夏令时规则。
- 改变时区开关会取消旧自动探针并重新安排检测。已有路由保持不变，新设置下需连续两次一致结果才切换，避免混用开关改变前后的确认次数。

## 实际边界

该功能改变的是发给模型的时间上下文，不会更改操作系统时区，也不代表改变了 OpenAI 服务端时区、账户地区或模型能力；不保证改善降智检测结果。没有加入未经证实的专用时区 HTTP 头。

IP 定位是近似值。若代理按目标域名分流，访问 ipapi.co 的出口可能与访问原生 Codex 或 BPS 上游的出口不同；若代理轮换出口，同一代理配置下的缓存最多可能滞后 6 小时。因此本功能不能保证每次模型请求都与地理定位服务观察到相同 IP。

## 实现依据

核对的是 OpenAI 官方 openai/codex 源码固定提交 41f9084b30812db321a0b592def4f500d1e79cf4；这是客户端环境上下文表示方式，不是对私有服务端接口行为的保证：

- [environment.rs](https://github.com/openai/codex/blob/41f9084b30812db321a0b592def4f500d1e79cf4/codex-rs/core/src/context/world_state/environment.rs)：198–205 定义 user 环境上下文类型，296–301 渲染 current_date 和 timezone。
- [turn_context.rs](https://github.com/openai/codex/blob/41f9084b30812db321a0b592def4f500d1e79cf4/codex-rs/core/src/session/turn_context.rs)：830–837 成对取得本地日期及 IANA 时区；原始客户端取本地时区，本插件按用户配置改用出口定位时区。
- [models.rs](https://github.com/openai/codex/blob/41f9084b30812db321a0b592def4f500d1e79cf4/codex-rs/protocol/src/models.rs)：960–974、1021–1035 定义每条消息内 content_item_kinds 与 content 的对应关系。
- [ipapi 官方接口文档](https://ipapi.co/api/)：timezone 单字段接口返回访问者 IP 对应的 IANA 时区文本；错误或额度限制按失败处理。

## 本地验证

通过 Go 全量回归、go vet、时区及自动检测集成测试；UI 267 项测试和 Chrome 沙箱/窄屏检查通过，Python 工具 40 项测试通过。网络用严格模拟传输覆盖，未调用真实 IP 查询或使用真实账号验证。本机关闭 CGO 且无 C 编译器；发行门禁要求同一提交的 Linux CI 完成 attachments、protocol、transport 三包 race 检查，实际结果见 GitHub Release。
