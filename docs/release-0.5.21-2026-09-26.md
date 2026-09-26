# 0.5.21：一键检测降智账号 · 渠道监控探活直通

适用于 Sub2API 官方版的插件，通过 OpenAI Excel（Basis Points）官方入口使用账号可用的官方模型。账号调度、令牌生命周期及用量计费由 Sub2API 负责。

本插件由第三方维护，适配官方版宿主，不是 OpenAI 或 Sub2API 官方发布。

## 本次更新

**一键检测降智账号（配置页新增按钮）**

- 对每个**当前可调度**的 OpenAI OAuth 账号发一次真实 Responses 请求，判据是“不联网，不猜测，直接说出你知道的最新苹果手机。只输出手机型号，不要解释。”
- 回答归一化（去空白、连字符、下划线并转小写）后包含 `苹果17`、`iPhone 17` 或 `Apple 17` 才判为正常；更早的世代、含糊或答非所问一律记为降智。
- 不可调度账号标记为跳过，不发出请求。并发上限 8，单账号超时 8 秒，整体受宿主 `config.test` 的 30 秒步进式验证窗口约束。
- 检测忽略当前账号白名单 —— 这项操作的目的正是发现该把哪些账号写进白名单。
- 检测完成后页面自动把降智账号勾选并保存；**没有降智账号时保留原有选择**，不会写出空数组（空数组在插件语义里等于“不限制账号”，误写会让全部账号走插件路由）。
- 触发位 `degradation_check` 只随 `config.test` 走一次，成功失败都会清零，不会在宿主重启后重复检测。

**渠道监控探活不再走 Basis Points**

- 宿主的渠道监控会发送带固定算术 challenge 的真实请求。即使它使用了插件已勾选的模型，也原样透传到宿主上游。
- 目的：探活不消耗 OAuth 账号的 BPS 额度，也不会把 BPS 的 429 反馈成账号限流。识别只匹配请求体顶层 `messages`/`input` 中的固定标记，普通用户问算术题不受影响。

其余路由、工具、图片与流式行为与 0.5.20 一致。

## 验证结果

- `go test ./... -skip TestLive -count=1 -timeout 120s`：全部包通过（attachments、auth、config、imagerelay、protocol、transport）。
- `go vet ./...`：无输出。
- `node --check ui/assets/bridge-v1.js`、`node --check ui/assets/app.js`：通过。
- `node --test tools/ui.test.cjs`：**118 项全部通过**（0.5.20 为 115）。新增用例覆盖降智检测的按钮接线、降智账号回填、无降智账号时保留原选择、以及触发位清零。
- Go 侧新增：`TestIsChannelMonitorProbeRecognizesOnlyTheTopLevelProbePayload`、`TestChannelMonitorProbeBypassesBasisPointsEvenForEnabledModel`、`TestDegradationModelFollowsEnabledModel`、`TestDegradationAnswerUsesStrictApple17Sentinel`、`TestDegradationCheckReturnsResultsAndOnlySelectsWrongAnswers`。

**未完成的验证**：本次没有用真实 OAuth 账号跑通“一键检测”的端到端流程（需要宿主环境与账号额度），也没有对渠道监控探活做真实宿主联调。命中判定、账号枚举回退、错误分类和 UI 收尾逻辑由上述单元测试与界面测试覆盖，真实上游行为仍需部署后确认。

## 安装包

Windows/Linux amd64 签名安装包：`dist/local.oai-basispoints-0.5.21.s2plugin`，13,101,189 字节。

- 包内成员：`manifest.json`、`signature.json`、`runtimes/windows-amd64/oai-basispoints.exe`（16,393,216 字节）、`runtimes/linux-amd64/oai-basispoints`（15,888,544 字节）、`ui/index.html`、`ui/assets/app.css`、`ui/assets/app.js`、`ui/assets/bridge-v1.js`。
- 沿用已发布的 Ed25519 签名身份 `oai-basispoints-v1`，公钥与 0.5.17/0.5.20 发布版一致：`MDM4N4ECbEAP8RVhRrDw8qdfCEtBV5xA4KWkZoBluZE=`。
- 用配套公钥独立验包（`tools/verify_package.py`，不复用打包器自检）通过：清单键集合、`requires` 键集合、逐文件 SHA-256、未声明文件、runtime/ui 入口、协议版本与清单原始字节的 Ed25519 签名均一致。
- 包内 4 个 `ui/` 文件与本次工作区文件哈希逐一相同。

SHA-256：`6ca657bc7e793f4b62c54d474398336d2e1f036ee313ddc00288ceb085cb71ba`。

发布附件包含签名安装包、SHA-256 校验文件、`publisher.public` 和源码压缩包。私钥及账号凭据不随源码或附件发布。
