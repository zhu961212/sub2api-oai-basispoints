# Sub2API Basis Points 传输插件

**交流群：1107265919**

[GitHub 仓库](https://github.com/zhu961212/sub2api-oai-basispoints) · [发布下载](https://github.com/zhu961212/sub2api-oai-basispoints/releases)

当前版本：**0.5.22** · 插件 ID：**local.oai-basispoints**

适用于 Sub2API 官方版的插件，通过 OpenAI Excel（Basis Points）官方入口使用账号可用的官方模型。插件为 OpenAI OAuth 出站请求提供传输适配：改写客户端工具目录，还原工具调用与 Responses/SSE 响应。账号调度、Token 生命周期、用量统计及计费仍由宿主负责；插件不刷新或持久化令牌。

本项目为第三方适配代码，不代表 OpenAI 或 Sub2API 的官方发布、授权或支持。

## 0.5.22 更新

- 修复 Basis Points 的 HTTP 429 被宿主误记为 Codex 全账号限流：普通请求、带图片请求和附件上传限流统一返回插件错误，不触发宿主账号冷却或自动换号。真实 Codex 透传的 429 仍保留原有限流保护。
- 兼容官方宿主，无需修改或重新编译宿主。当前官方宿主把这类插件错误返回为 502（流式请求可能显示失败事件），宿主诊断中的错误码为 PLUGIN_RATE_LIMITED，说明中保留原始 HTTP 429。BPS 配额仍需等待上游恢复。
- [0.5.22 修复记录与升级说明](docs/release-0.5.22-2026-09-26.md)。
- 删除固定算术 challenge 的提示词扫描与特殊绕行，监控请求与普通请求统一遵循模型选择和账号白名单。
- 一键检测改用正式请求协议，限制整体扫描时间；429、超时、空白或未完成回答不再被误选为疑似降智，保存失败保留原账号选择。六模型真实转发及一键检测已通过，详见修复记录。

## 0.5.21 更新

- 配置页新增**一键检测降智账号**：给每个当前可调度的 OpenAI OAuth 账号发一次真实请求，答案不指向“苹果17”的账号判为降智；检测结束后自动把降智账号保存为账号白名单，没有降智账号则保留原有选择。
- 当时增加了渠道监控 challenge 绕行；该临时规则已在 0.5.22 删除，由统一的 BPS 429 隔离替代。
- [0.5.21 发布记录](docs/release-0.5.21-2026-09-26.md)。

## 0.5.20 更新

- 模型选择按版本分两排：上排为 GPT-6（Astra、Sol、Luna），下排为 GPT-5.6（Sol、Terra、Luna）；默认仍勾选 Astra 和 5.6 Sol。
- [0.5.20 发布记录](docs/release-0.5.20-2026-09-26.md)。

## 0.5.19 更新

- 配置页增加 6 个模型选择：`gpt-6-astra`、`gpt-5.6-sol`、`gpt-6-sol`、`gpt-6-luna`、`gpt-5.6-terra`、`gpt-5.6-luna`。
- 默认只勾选 `gpt-6-astra` 和 `gpt-5.6-sol`；保存后重新读取确认模型与账号选择。
- 未勾选的模型继续走宿主上游。清空模型选择表示全部透传，清空账号选择仍表示不限制账号。
- [0.5.19 发布记录](docs/release-0.5.19-2026-09-26.md)。

## 0.5.18 更新

- 新增 `gpt-5.6-sol` 的 Basis Points 路由，与 `gpt-6-astra` 一样按账号白名单生效，上游保留各自模型名。
- 模型名去除首尾空白后精确匹配；其他模型和旧 `-excel` 别名继续原样透传，账号选择规则不变。
- [0.5.18 发布记录](docs/release-0.5.18-2026-09-26.md)。

## 0.5.17 更新

- 缓存增加容量、字节预算与过期回收，优先保留常用会话；修复工具历史缓存的大整数精度。
- 账号目录合并并发刷新，失败短暂退避；保存账号或工具选项时保留已有连接池。
- 图片缓存跳过已校验内容的重复扫描，同请求重复图避免重复摘要计算；匿名请求不占用会话缓存。
- 插件介绍与本仓库统一提供交流群：**1107265919**。
- [缓存策略与本地性能](docs/CACHE.md) · [0.5.17 发布记录](docs/release-0.5.17-2026-09-26.md)。

## 请求范围

只有**命中宿主插件灰度、符合账号白名单且模型已在配置页勾选**的请求转发到 Basis Points。支持选择上面列出的 6 个模型，模型名去除首尾空白后精确匹配，上游保留各自模型名。

| 情况 | 处理方式 |
| --- | --- |
| 已勾选模型，符合账号白名单 | 转发到 Basis Points，保留请求模型名 |
| 白名单外账号 | 原样透传到宿主指定的上游 |
| 未勾选的模型、其他模型及旧 -excel 别名 | 原样透传到宿主指定的上游 |
| 未勾选任何模型 | **全部透传**到宿主指定的上游 |
| 未勾选任何账号 | **不限制账号**，不是禁用插件；仍需满足模型与灰度条件 |

凭据与代理始终来自宿主本次调度的同一个账号。插件不替换或轮询账号。模型选择由 `enabled_models` 保存，默认启用 Astra 和 5.6 Sol，显式空数组表示全部透传。

## 宿主要求

清单要求 Sub2API **>=0.2.8 <0.3.0**，推荐版本为 0.2.8，Plugin Protocol、Transport API、UI Bridge 均为 1。该范围对应 Wei-Shaw/sub2api 的 0.2.8 版本线，其他 fork 的 2.8.12 不等同于 0.2.8。完整版本契约和已有实测范围见 [兼容说明](docs/COMPATIBILITY-0.2.8.md)。

## 开发环境

- **Go 1.27 或更新版本**：以 go.mod 的 go 1.27 为准。
- **Node.js 18 或更新版本**：运行内置测试，无需 npm install。
- **Python 3**：独立验包工具仅使用标准库。
- **Chrome / Edge / Chromium**：运行可选的浏览器回归，可用 CHROME_PATH 指定可执行文件。
- Windows 使用 PowerShell；Linux 使用 Bash。

以下命令均在项目根目录执行。首次构建可能需要下载 Go 工具链与模块；“离线测试”指不访问真实 Basis Points 账号，不代表无需准备依赖。

## 测试

运行不访问真实上游的回归：

~~~powershell
go test ./... -skip TestLive -count=1 -timeout 120s
go vet ./...
node --check ui/assets/bridge-v1.js
node --check ui/assets/app.js
node --test tools/ui.test.cjs
~~~

另行运行浏览器回归：

~~~powershell
node tools/test-ui-browser.mjs
~~~

浏览器回归使用本地模拟宿主，验证 iframe 沙箱下的选择、保存、重新读取及全选行为，不连接生产数据库。具备 CGO 与 C 编译器的环境可额外运行 go test -race ./... -skip TestLive。

TestLive 系列属于可选真实上游测试，设置 BASISPOINTS_LIVE_TOKEN 与 BASISPOINTS_LIVE_ACCOUNT_ID 后会消耗账号额度。不要在公开日志、源码或 Issue 中填写真实值；运行方式和限制见 [使用说明](docs/使用说明.md)。

## GitHub 源码准备

运行 python -X utf8 tools/export_source.py，生成 dist/sub2api-oai-basispoints-0.5.22-github-source.zip 及 SHA-256 文件。源码包包含当前源码、测试、CI 和文档，不包含 Git 历史、构建产物或发布密钥。解压后按 [GitHub 上传说明](docs/GITHUB.md) 上传。

## 构建、签名与验包

首次发布时，在**仓库外**生成自己的 Ed25519 发布密钥。已有密钥应复用，不要重复执行生成命令覆盖它：

~~~powershell
go run ./tools/keygen -out ../basispoints-private/publisher
~~~

Windows：

~~~powershell
.\build.ps1 -SigningKey ..\basispoints-private\publisher.private -KeyId my-publisher-v1
~~~

Linux：

~~~bash
bash ./build.sh -signing-key ../basispoints-private/publisher.private -key-id my-publisher-v1
~~~

构建脚本明确跳过 TestLive。默认生成 Windows/Linux amd64 包：dist/local.oai-basispoints-0.5.22.s2plugin。build/ 和 dist/ 为本地生成目录，不随源码上传。无签名参数时会生成未签名包，仅用于允许未签名插件的本地调试。

显式传入配套公钥进行独立验包：

~~~powershell
python -X utf8 tools/verify_package.py dist/local.oai-basispoints-0.5.22.s2plugin --public-key ../basispoints-private/publisher.public
~~~

校验器检查包内容、文件哈希及 manifest 原始字节的 Ed25519 签名。构建脚本发现 Python 时会自动验包，但自动验签取决于其默认公钥路径；使用仓库外密钥时仍应执行上面的显式验签命令。

部署者需将公钥配置到宿主，并使用与打包一致的 key_id：

~~~yaml
plugins:
  allow_unsigned: false
  trusted_publishers:
    my-publisher-v1: BASE64_ED25519_PUBLIC_KEY
~~~

在 Sub2API 插件管理页安装 0.5.22，打开配置页选择模型和账号并保存，再配置灰度并启用插件。详细步骤见 [使用说明](docs/使用说明.md)。

## 配置页与全选

在配置页勾选允许走 Basis Points 的模型和账号，点击保存；显示“已保存，并已重新读取确认”后生效。默认模型为 `gpt-6-astra` 和 `gpt-5.6-sol`，其余 4 个可按需勾选。

- **全选列表账号**：一次勾选当前列表账号，并保留已保存但暂未显示的账号；点击全选后仍需保存。
- **一键检测降智账号**：点击后，遍历所有当前可调度的 OpenAI OAuth 账号，不受原账号白名单限制，使用已勾选模型中的第一个（未选时使用默认 Astra）请求配置的 BPS Responses 端点，不检测原生 Codex。每个请求使用该账号的凭据和关联代理，无代理时直连。有效回答指向“苹果17”才通过现有自定义规则；其余有效回答标记为疑似降智，自动勾选并保存为插件账号白名单。401/403/429、超时、空白或未完成回答只记为检测失败，不会被选中。没有疑似降智账号时保留原选择，保存失败时恢复原选择。该功能由按钮触发，不在后台定时扫描；每个被检测账号消耗一次上游请求额度。
- 读取、保存及回读期间锁定操作；列表为空或已全选时禁用全选按钮。
- 重复账号按有效 ID 去重；保存失败时保留当前选择，可重试。
- 清空账号勾选表示**不限制账号**；清空模型勾选并保存则表示**全部透传**。也可在宿主停用插件或调整灰度。

账号的添加在宿主账号管理页完成。高级字段的默认值与行为见 [使用说明](docs/使用说明.md)。

## 发送图片

0.5.16 起，客户端直接发送图片和截图即可，**无需任何图片配置**。插件使用宿主当前调度账号及其代理，自动将内嵌 Base64 图片上传至 BPS 原生附件接口，再以返回的 file_id 交给 **BPS 上已勾选的本次请求模型**。配置页提供模型与账号选择。

支持 PNG/JPEG/GIF/WebP，以及消息内容、function/custom 工具结果中的截图。原有 HTTPS 图片地址和有效 file_id 保留；图片原始字节、尺寸和 detail 不做压缩或降级。支持 auto/low/high，detail=original 仍返回明确 400。单张最多 20 MiB、每请求最多 20 张、解码后合计 32 MiB，单张最多 64 Mi 像素。

原生附件使用流式 multipart 上传，不生成公网图片下载链接、不启动图片监听、不把图片写入本地文件。请求内重复图片只上传一次；同可信会话、账号、令牌及端点下可复用最多 30 分钟的附件 ID 元数据。缺少可信会话标识时不跨请求复用。30 分钟仅是本地缓存期限，不代表上游文件的保留或可用期限。

无需填写域名、端口或目录，也无需添加反向代理。图片请求的资源边界、错误处理和协议来源见 [自动发送图片说明](docs/IMAGE-RELAY.md)。

0.5.15 的 [图片性能报告](docs/PERFORMANCE-0.5.15.md) 记录的是旧自托管路径，仅作为历史数据保留，不能用来证明当前原生上传的速度。图片相关的实际验证结果见 [0.5.20 发布记录](docs/release-0.5.20-2026-09-26.md)；尚未用真实 OAuth 账号验证上游图片识别。

## 常见工具目录错误

~~~text
stream disconnected before completion:
Basis Points returned an unknown client tool absent from the active catalog
~~~

该错误表示返回的调用无法匹配本轮客户端声明的工具目录。部分客户端的提示仍用 exec_command 固定示例，但本轮可能只声明 functions.exec 执行器。0.5.15 按实际目录生成调用格式指引，区分执行器与其内部 helper、自定义工具的原始 input 与中转信封中的 args。

更新后需在宿主重新加载插件，确认运行版本为 0.5.22，再重新发起请求。若目录只声明 functions.exec，执行器内部的 helper 必须通过该执行器调用，不能独立作为中继目标。自定义工具使用原始文本参数，插件再还原为客户端调用。

首次工具交互若上游仍返回一个目录外的中转调用，并且尚未向客户端发出任何工具，0.5.15 最多追加一次带当前目录与拒绝原因的纠正请求。它沿用原账号、模型、历史和图片引用；错误工具不会执行。纠正后的结果仍须通过原有目录和参数校验，两次请求的用量合并记录。

已有工具调用/结果的后续轮次、多个或位置不安全的工具、有歧义或参数非法的调用不会自动重试；纠正后仍无效会明确失败。客户端可能把失败终态显示为 stream disconnected，请结合返回的 code / message 和脱敏诊断定位。

## 源码目录

~~~text
cmd/oai-basispoints/       插件入口
internal/auth/             令牌只读解析与账号 ID 回退
internal/attachments/      BPS 原生图片附件上传与有界元数据缓存
internal/config/           默认配置、校验与归一化
internal/protocol/         工具改写、调用还原、Responses/SSE 协议
internal/transport/        出站请求、代理、心跳与流式处理
third_party/sub2api/       本地使用的宿主插件 API 契约副本
ui/                        配置页与 UI Bridge
tools/                     打包、密钥生成、验包、UI 回归工具
docs/                      使用、兼容、发布与上传说明
manifest.source.json       插件清单源文件
build.ps1 / build.sh       构建入口
go.mod / go.sum            Go 依赖与校验信息
~~~

third_party/sub2api 被 go.mod 的本地 replace 引用，是构建所需项目文件，不应当作缓存删除。

## 文档与发布

- [完整使用说明](docs/使用说明.md)：安装、配置、客户端使用和排查。
- [兼容说明](docs/COMPATIBILITY-0.2.8.md)：宿主契约、版本范围和历史实测。
- [0.5.16 发布记录](docs/release-0.5.16-2026-09-26.md)：自动原生发图、配置迁移与验证范围。
- [0.5.14 发布记录](docs/release-0.5.14-2026-09-25.md)：图片发送修复与本地验证范围。
- [0.5.15 发布记录](docs/release-0.5.15-2026-09-25.md)：首轮工具调用修复与图片性能优化。
- [历史图片性能报告](docs/PERFORMANCE-0.5.15.md)：仅适用于 0.5.15 的自托管中转路径。
- [流式与数值参数回归记录](docs/regression-2026-09-25-stream-numeric.md)：专项验证。
- [GitHub 上传说明](docs/GITHUB.md)：源码边界、上传步骤及 Release 附件。

历史发布记录描述当次验证和产物，不保证本地当前存在相应安装包，也不代表已部署或通过真实上游实测。

## 许可

项目自有代码目前尚未设置开源许可证，公开仓库不等于授予 MIT 或其他许可证。公开发布前需由权利人确认自有代码的许可证；第三方代码来源及适用声明见 [第三方说明](THIRD_PARTY_NOTICES.md)。
