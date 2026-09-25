# Sub2API 0.2.8 兼容记录

目标宿主是**官方 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 0.2.8 线**（`backend/cmd/server/VERSION` = `0.2.8`）。

契约来源为提交 `a3eb7ef302961cba716dc78b39b93b60c467db0e`（`chore: sync VERSION to 0.2.8`）。2026-09-25 再次将本仓库的五份 API 文件与该提交的官方工作副本比对，归一化行尾后内容一致。仓库不包含宿主源码 checkout；来源与许可见 [第三方声明](../THIRD_PARTY_NOTICES.md)。

## 清单要求

| 字段 | 取值 |
| --- | --- |
| `requires.sub2api` | `>=0.2.8 <0.3.0` |
| `requires.recommended_sub2api_version` | `0.2.8` |
| `requires.tested_sub2api_versions` | `["0.2.8"]` |
| `plugin_protocol` / `transport_api` / `ui_bridge` | `1` / `1` / `1` |

宿主的版本范围解析器只接受空格或逗号分隔的比较条件，全部按 AND 组合，不支持 `||`、`^`。宿主用语义化版本比较，`0.2.8` 与 `2.8.12` **不是同一条线**（后者是 ranxi2001 fork 的版本号，见文末）。

## API 与包格式

插件 `third_party/sub2api/pkg/pluginapi/v1/` 的五份文件与官方 0.2.8 **内容完全一致**：

| 文件 | 比对结果 |
| --- | --- |
| `plugin.pb.go` | 逐字一致（LF） |
| `plugin_grpc.pb.go` | 逐字一致（LF） |
| `runtime.go` | 逐字一致（LF） |
| `plugin.proto` | 内容一致，仅 checkout 行尾不同（宿主 CRLF / 插件 LF） |
| `manifest.schema.json` | 内容一致，仅 checkout 行尾不同（宿主 CRLF / 插件 LF） |

行尾差异不影响编译、协议解析或安装校验；机械比对时应先归一化行尾，插件仓库用 `.gitattributes` 固定为 LF。

| 契约 | 0.2.8 要求 | 检查结论 |
| --- | --- | --- |
| 插件握手 | Plugin protocol 1 | 一致（`runtime.go` 逐字相同，`ProtocolVersion = 1`） |
| 转发 API | Transport API 1 | 一致 |
| 配置页 | UI Bridge 1 | 一致 |
| 宿主反向服务 | HostService API 2 | 一致；提供账号目录与出站身份 |
| 包格式 | ZIP + `manifest.json` + 文件 SHA-256 + 运行平台声明 | 无格式变更 |
| 签名 | `signature.json`，Ed25519 对原始 manifest 字节签名 | `key_id` 必须匹配部署配置的 `trusted_publishers` |

官方 0.2.8 **没有** Basis Points 服务实现（`backend/internal/service/basispoints/` 是 fork 加入的），插件补的正是官方缺失的这条出站通路。

## 宿主侧实测（0.5.5）

把临时探针放进宿主 `backend/internal/service/`，用宿主自身的 `EvaluatePluginCompatibility`、`PluginPackageInstaller`、`startPluginRuntime`、`PluginManager.SaveConfig/GetConfig` 复验签名包（跑完即删，不修改宿主任何生产文件）：

| 测试 | 结果 |
| --- | --- |
| 兼容性判定 | 通过：`0.2.8` → `status=compatible`、`tested=true`、`required=>=0.2.8 <0.3.0`；`0.2.7`、`0.3.0`、`2.8.12` 均被判为不兼容 |
| 契约比对 | 通过：五份文件与宿主源码内容一致（忽略 checkout 行尾） |
| 安装 | 通过：`local.oai-basispoints 0.5.5`，`signature=trusted`（公钥配进 `trusted_publishers` 后判定）、包内文件哈希校验、默认 `state=disabled` |
| 运行时 | 通过：**真实启动插件子进程**并完成 go-plugin 握手，`GetInfo` 的插件 ID / 版本 / 协议 / 传输 API 与清单逐字段一致，`Health` 健康 |
| 配置 | 通过：`SaveConfig({"account_ids":[11,22]})` 后 `GetConfig` 往返一致；非法配置 `{"timeout_seconds":1}` 被拒且不覆盖既有设置 |

环境变量（探针用）：`BASISPOINTS_PLUGIN_ROOT`、`BASISPOINTS_SIGNED_PACKAGE`、`BASISPOINTS_PUBLIC_KEY_FILE`、`BASISPOINTS_PUBLIC_KEY_ID`。

签名包独立校验（`tools/verify_package.py --public-key build/keys/publisher.public`，不复用打包器代码）：清单键集合、`requires` 键集合、6 个声明文件的 SHA-256、运行时入口与 UI 入口全部一致，`manifest.json` 原始字节验签通过。

测试使用合成账号元数据和内存配置仓库，不读取真实账号 token，不请求 BPS，不等同于生产数据库或外网端到端测试。Windows 进程测试也不代表 Linux 二进制已经实际运行验证。

## 宿主行为与已知边界

- 宿主将本次实际使用的代理放在 `ForwardRequestStart.proxy_url`；本插件始终使用宿主当前调度账号的凭据、身份头和代理。头部没有凭据时，仅为同一账号调用 `ResolveOutboundIdentity`。
- 空代理代表直连，不应由进程环境变量决定额外代理。
- 配置页 `config.save` 会触发宿主二次验证，成功响应是规范化配置；插件随后使用 `config.load` 读回确认。空选择在后端规范化后可为 `account_ids: null`，UI 将其正确解释为未选择。
- 旧 `account_id` 的兼容转换在配置页保存流程中进行；后端严格解析不会直接接受旧字段，不应把它描述为后端自动迁移。
- 当前版本的账号选择是白名单，插件不替换账号、不做轮询；是否可调度由宿主决定。`ResolveOutboundIdentity` 用于解析当前账号身份，不构成账号调度或可用性保证。

## 附：ranxi2001 fork 的 2.8.12 线

曾对照 `github.com/ranxi2001/sub2api` 的 `2.8.12` 版本线，其 `backend/internal/service/basispoints/` 包含 Basis Points 移植实现（移植自 hloolx/codex2api）。参考工程不随本仓库分发，也不是构建依赖。

- **它不是本插件的目标宿主**。清单按官方 0.2.8 线声明，在该 fork 上会被判为不兼容（`>=0.2.8 <0.3.0` 匹配不到 `2.8.12`）。
- 若确实需要在两条线都能安装，只能把 `requires.sub2api` 写成无上界的 `>=0.2.8`（宿主匹配器不支持 `||`）；代价是未来任何大版本都会被判为"范围兼容但未测试"。
- 那份 fork 实现仍是协议细节的参考来源：本次计划工具归一化就是对齐它的 `plan.go` 语义。
