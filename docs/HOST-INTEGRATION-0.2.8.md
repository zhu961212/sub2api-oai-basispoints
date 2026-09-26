# 宿主调用链与插件边界（2026-09-26）

本记录依据本地 `E:/sub2api` 源码，`backend/cmd/server/VERSION` 为 `0.2.8`，HEAD 为 `a3eb7ef302961cba716dc78b39b93b60c467db0e`。本地工作副本包含额外未提交文件，因此下述结论以列出的实际实现为准，不外推到其他部署或版本。

## 1. 添加账号与 OAuth 授权

实际路径：

```text
CreateAccountModal.handleOpenAIExchange
  -> useOpenAIOAuth.exchangeAuthCode
  -> OpenAIOAuthService.ExchangeCode
  -> 独立 oauthClient 兑换 token
  -> buildCredentials / buildExtraInfo
  -> adminAPI.accounts.create
  -> adminServiceImpl.CreateAccount / 账号仓库
```

- `backend/internal/service/openai_oauth_service.go` 的 `ExchangeCode` 检查授权会话与 state，使用原会话的 PKCE verifier、redirect URI 和代理兑换 token。
- `backend/internal/repository/openai_oauth_service.go` 使用独立 HTTP 客户端访问 token 端点，不调用插件。
- `backend/internal/service/admin_account.go` 的 `CreateAccount` 创建账号；它不经过插件 `Forward`。
- 因此，提交授权码时失败、账号尚未创建，不能仅凭插件版本判断是 BPS 转发造成的。需区分 state 校验、token 兑换、账号字段校验与数据库创建。

此次已修复两个前端边界问题（仅本地源码）：

1. `OAuthAuthorizationFlow.vue` 原先重新生成链接时只清空输入、未清空旧回调 state。旧完整回调链接 -> 重新生成 -> 粘贴新裸 code 的组合继续使用旧 state。现在新会话、重新生成和替换输入清理旧 state；自动从当前完整 URL 提取 code 时仍保留该 URL 自身的 state。
2. `CreateAccountModal.vue` 的授权外层 catch 原先读取 Axios `response.data.detail`，而 `frontend/src/api/client.ts` 已将错误规范化为含 `message` 的普通对象。现在使用已有 `extractApiErrorMessage`，保留失败接口的真实原因。

生产定位所需的最小证据是失败请求路径、HTTP 状态、`reason` / `message`，以及失败发生在兑换 token 还是创建账号。不要提供授权码、token、Cookie、完整回调 URL 或其他凭据。

## 2. 插件何时接管

`backend/internal/service/plugin_manager.go`：

- `ShouldRouteOpenAIOAuth` 要求非空账号对象、`platform=openai`、`type=oauth`、已发布路由及命中账号 ID 的稳定灰度桶。此函数不检查 ID 是否大于零；不能把对象存在直接等同于已入库，OAuth 兑换不走插件的结论来自实际调用点。
- `RoundTripOpenAIOAuth` 接管后不会因为插件错误自动回到原生传输；插件必须正确完成未选模型/账号的透传。
- `openai_plugin_transport.go` 将真实推理和账号测试交给该接口；模型目录查询也可能进入插件。OAuth 授权码兑换不在这些调用点中。
- 插件不能将任意宿主 HTTP 请求仅因 JSON 中有模型字段就视为 BPS Responses 请求；扩展路由规则时需核对方法、端点和宿主用途。

宿主仍负责账号调度、凭据生命周期和代理。插件接管与账号是否可调度不是同一概念，也不能用插件账号列表替代宿主调度判断。

### 反向服务和生命周期

- `plugin_host_services.go` / `openai_plugin_account_directory.go` 中的 `ListAccounts` 返回能力 scope 内的账号，不按 rollout 再过滤。暂停或限流中的 active 账号仍会出现，`Schedulable` 才是宿主可调度判断。
- `ResolvePluginOutboundIdentity` 不检查 schedulable；它调用 `GetAccessToken`，可能触发 token 刷新，过期且缺少 refresh token 时甚至会持久化账号错误。它不是完全无副作用读取，主动检测必须先尊重 `Schedulable`。
- `Health` / `status_json` 必须被动生成；不得发起上游探测或应用配置。首次 Health 发生在 InitHostServices 之前，不能依赖已经有账号目录。
- `config.save` 验证并 Apply 后才持久化，持久化失败会 Apply 旧配置；`config.test` 还会再次 Apply 当前已保存配置。两者在插件停用时也可能使用临时 runtime，不能用作普通状态轮询或在 Apply 中重复启动探测。
- UI 应用 `plugin.status` 轮询状态；它不会启动停用的 runtime。UI bridge 不提供创建账号、修改账号限流或任意管理接口。

## 3. 429 不只来自 HTTP 状态

宿主 `openai_gateway_passthrough.go` 的 `openAIStreamFailedEventSemanticStatus` 从以下位置判定账号错误：

- HTTP 200 的 `error` / `response.failed` 事件；
- 顶层、`error`、`response.error` 中的 `status` / `status_code`；
- 错误 code、type、message 中的 `rate_limit`、认证与权限标记。

`handleOpenAIStreamTerminalAccountSideEffects` 会将识别到的 401/403/429 交给宿主账号状态处理。`openAIStreamFailedEventShouldFailover` 还可在尚无实际输出时触发换号。仅拦截 HTTP 429 不足以隔离 BPS 配额。

`openai_gateway_usage.go` 的 `ParseCodexRateLimitHeaders` 会读取 `x-codex-primary-*` / `x-codex-secondary-*` 等配额头；`UpdateCodexUsageSnapshotFromHeaders` 可异步写入账号 Extra。BPS 响应中的这些字段不应被当作原生 Codex 配额。会话及协议相关头应保留，不能无差别删除所有 `x-codex-*`。

## 4. 错误契约与修复原则

- `plugin_runtime.go` 将响应开始前的 `ForwardResponseError` 转成 `PluginTransportError`。`request_sent=true` 告诉宿主不能安全重放已发出的请求。
- 原生 Codex 透传的 HTTP 429、正文、重试与配额头必须保持，让宿主执行真实限流保护。
- BPS HTTP 错误、SSE 内错误、JSON 失败、附件上传和工具纠正请求应使用一致的服务级错误边界。保留失败和可读原因，不能伪造成功。
- 仅改 SSE 错误 code 仍可能触发宿主按 type/message/status 识别限流；改写后应通过宿主真实分类器验证。
- 修复必须同时验证“不写宿主账号状态”和“不无意义遍历全账号池”。
- 不自动清除宿主已有冷却或错误状态。其来源未确认时，清除可能破坏真实 Codex 限流保护。

## 5. 验证范围

本地回归结果：

- 真实父子 Vue 组件集成用例在旧实现上复现 3 个失败；修复后连同相关测试共 45 项通过，类型检查及修改文件 ESLint 通过。
- 插件完整离线 Go 测试与 174 项 UI 回归通过。BPS 的 HTTP 401/403/429、HTTP 200 JSON/SSE 内失败、图片路径、原生透传和工具纠正路径均有针对性覆盖。
- 使用 Go overlay 将测试放进宿主 service 包，不修改宿主后端：原始 401/403/429 信号确实进入宿主分类；隔离后的 error/response.failed 不触发换号，也不写账号限流、错误、Extra 或临时禁调。
- 正常输出、usage、会话头和原生 Codex 429 继续保留。此次没有扩展 HTTP 402 等未确认的 BPS 状态语义，也没有清除任何真实账号状态。

本记录是源码学习与本地回归的依据，不代表已访问生产日志、已复现用户服务器或已部署修复。OAuth 失败的具体生产原因仍需失败接口的非敏感错误信息确认。
