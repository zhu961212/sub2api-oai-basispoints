# Sub2API 官方 0.2.8 / 0.2.9 兼容记录

核查日期：2026-09-28。目标为 **Wei-Shaw/sub2api 官方 Release**，不是使用其他版本号的 fork。

## 固定来源

| 官方版本 | Release tag | tag 对象 | 实际源码提交 |
| --- | --- | --- | --- |
| 0.2.8 | v0.2.8 | d7a82d78ca51d42be41cb4daa3510ea401defe9f | fd80b08c90b55edcad5b00171b53f08721d30da1 |
| 0.2.9 | v0.2.9 | 8532ec28b56188d3c845ed97070e6ee5136bd9bd | 4c00df2e0183e2c70b7fa8ba45914205e36aad0c |

官方 0.2.9 Release 发布时间为 **2026-09-28 03:08:59 UTC（北京时间 11:08:59）**。来源：

- https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.8
- https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.9
- https://api.github.com/repos/Wei-Shaw/sub2api/releases/tags/v0.2.9

本次使用 git ls-remote 确认官方 tag 和剥离后的 commit，fetch 到本插件仓库独立的 refs/bps-compat/v0.2.8、refs/bps-compat/v0.2.9，再由 git archive 导出精确源码。未 checkout、重置或修改现有宿主工作区，宿主工作区的本地补丁没有进入验证样本。

### Release 版本与源码 VERSION 文件

两个 Release tag 中的 backend/cmd/server/VERSION 分别仍是 0.2.7 和 0.2.8。这是上游发布流程的正常时序：

1. .github/release-tools/release_matrix.py 的 plan 从 Release tag 解析版本、核对 tag commit，并写入 VERSION 文件。
2. .github/workflows/release.yml 把该文件作为 version-file artifact 交给各平台构建。
3. 发布成功后，sync-version-file job 再向默认分支提交版本同步。

因此应以官方 Release tag、commit 和正式构建版本判断 0.2.8 / 0.2.9，不能仅凭 tag 内未更新的 VERSION 文件识别发布版本。兼容性测试把实际 Release 版本传入官方 PluginHostInfo；这与宿主正式构建的版本语义一致，不需要改动被测生产源文件。旧记录使用的 a3eb7ef302961cba716dc78b39b93b60c467db0e 是 0.2.8 发布后的版本同步提交。

## 协议与实现对比

插件 third_party/sub2api/pkg/pluginapi/v1/ 的五份文件与两个官方 tag **逐项一致**，仅归一化 CRLF/LF 行尾：

| 文件 | 两个官方版本及插件副本的 SHA-256（LF） |
| --- | --- |
| plugin.pb.go | 6af7456f7208f180c9ee081a32bbbc029234e855b778b68ac64c1c33be954a63 |
| plugin_grpc.pb.go | a787c75a3c95b81483ead670b9f74b52d092c7bb0652d1ffb8cc4f1667501d7e |
| runtime.go | 72b32af120f61617dec82bde6248082b8c39e99294925adc9184816bee06efe3 |
| plugin.proto | ee309c946e339b76523ed123e8963b788c0f4cafeffd58f399563bdd2d42ca69 |
| manifest.schema.json | d6e33c6074370629bc276f29864693a63f20109051f8c95f585dc7c28f9f4e4f |

下列生产路径在 v0.2.8 → v0.2.9 之间没有差异：

- backend/pkg/pluginapi/：协议定义、manifest schema、UI Bridge 文档。
- backend/internal/service/plugin*：兼容性判定、清单解析、包安装、签名验证、runtime、插件管理、HostService。
- backend/internal/service/openai_plugin*：OpenAI OAuth 出站插件接入和账号目录。
- backend/internal/handler/admin/plugin_handler.go、backend/internal/server/routes/admin.go：管理接口及路由。
- frontend/src/api/admin/plugins.ts、frontend/src/views/admin/PluginsView.vue：管理页 API 和 UI Bridge 分发。

旧 0.2.8 同步基线 a3eb7ef 到官方 v0.2.9 的上述插件核心协议和服务路径也无差异。无需复制宿主补丁或更新 vendored API 即可使用相同插件协议：Plugin protocol 1、Transport API 1、UI Bridge 1、HostService API 2。

包格式、文件 SHA-256、Ed25519 对原始 manifest 字节签名以及 trusted_publishers 信任配置保持一致。插件的 oai-basispoints-v1 公钥仍须由部署方按安装说明配置；宿主内置的其他插件发布者公钥不替代此信任设置。

0.2.9 同时改进了 OpenAI Beta 请求头保留、额度自动暂停/重置及计费等宿主行为。这些变化不改变插件握手、转发协议或包格式；账号调度、Token 生命周期及计费仍由宿主负责。相同 API 不能替代真实账号的生产上游验收。

## 清单声明

| 字段 | 取值 |
| --- | --- |
| requires.sub2api | >=0.2.8 <0.3.0 |
| requires.recommended_sub2api_version | 0.2.9 |
| requires.tested_sub2api_versions | ["0.2.8", "0.2.9"] |
| plugin_protocol / transport_api / ui_bridge | 1 / 1 / 1 |

范围内的其他 0.2.x 版本由宿主显示为“范围兼容但未测试”；0.2.7、0.3.0 和 fork 的 2.8.12 不在此范围内。tested 声明只对应本文明确列出的验证范围。

## 验证范围与本地证据

精确 tag 的隔离源码位于 build/compatibility-0.2.8-0.2.9/host-0.2.8 和 host-0.2.9。每个目录的 COMPATIBILITY_SOURCE.json 记录 tag、commit、原 VERSION 及 API 哈希；同级 contract-comparison.json 记录逐项一致性和空的宿主插件路径差异。这些 build 证据文件不作为源码包内容发布。

### 官方宿主专项测试

使用 Go 1.27.1（windows/amd64），在两份精确 tag 的 backend 下分别执行官方原有插件专项测试；生产源码保持原样，模块使用本地缓存、-mod=readonly，并设置 -count=1 避免命中测试结果缓存：

| 被测官方 tag | 结果（含子测试） | 跳过 |
| --- | --- | --- |
| v0.2.8 | 48 项通过，0 失败 | 0 |
| v0.2.9 | 48 项通过，0 失败 | 0 |

覆盖版本范围与协议判定、安装包签名/哈希/路径和体积约束、HostService 存储与 broker、账号范围和目录、UI token、配置规范化、RPC 发送状态与 failover 约束。测试命令和退出码见 build/compatibility-0.2.8-0.2.9/host-contract-test-results.json，逐项日志分别为 host-0.2.8-contracts.log、host-0.2.9-contracts.log。

这些是官方宿主契约及安全逻辑测试；最终插件签名包安装及真实子进程回放是另一个验证层，其包 SHA-256、宿主 commit 和结果在对应 GitHub Release 正文及本地 proof 中另行记录。不能把 Windows 进程实测描述为 Linux 二进制运行验收，也不能把合成账号和 loopback 上游测试描述为真实账户调用验证。

历史 0.2.8 的细节见 [原始兼容记录](COMPATIBILITY-0.2.8.md)；第三方副本的原始来源与许可仍见 [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md)。
