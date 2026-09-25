# Excel / Basis Points 模型实测

测试日期：2026-09-26。使用用户提供的单个 OpenAI OAuth 账号及其导出代理配置；凭据只在进程内存中使用，记录不包含令牌、账号标识或代理地址。

## 结果

该账号的模型目录返回 `allowed=true`、`source=chatgpt`、`restricted_models=[]`，默认模型为 `gpt-5.6-sol`。目录列出的 6 个模型均完成一次 `low` 思考等级的短文本请求，HTTP 200，终态为 `completed`，回复包含 `OK`，返回模型名与请求一致。

| 模型 | 验证路径 | HTTP | 单次耗时 |
| --- | --- | --- | --- |
| `gpt-5.6-sol` | 0.5.18 插件完整 Forward 路径 | 200 | 3.448 秒 |
| `gpt-6-astra` | 0.5.18 插件完整 Forward 路径 | 200 | 3.208 秒 |
| `gpt-5.6-luna` | 直接 BPS Responses 请求 | 200 | 2.110 秒 |
| `gpt-5.6-terra` | 直接 BPS Responses 请求 | 200 | 5.688 秒 |
| `gpt-6-luna` | 直接 BPS Responses 请求 | 200 | 2.000 秒 |
| `gpt-6-sol` | 直接 BPS Responses 请求 | 200 | 2.078 秒 |

这些耗时是单次短文本请求的完成时间，包含网络与代理开销，不构成性能排名。完整 Forward 测试还确认了客户端流的 `response.completed`、`[DONE]` 和插件 End 帧。直接上游流以 `response.completed` 结束，没有原始 `[DONE]`，插件负责补齐客户端终态。

0.5.18 的正式模型路由仍为 `gpt-6-astra` 与 `gpt-5.6-sol`。其他四个模型只做上游可用性验证。本次未测试工具执行、多轮会话或图片识别；结果仅代表这个账号在测试时的可用性。

## 模型目录来源

通过 [Excel 官方 Manifest](https://bps.openai.com/basispoints/api/office/manifest.xml) 定位当前前端，再从 [当前主脚本](https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/assets/x-square-DV9EmyFo.js) 确认 OAuth 客户端调用：

```text
GET https://bps.openai.com/basispoints/api/responses/access?include_models=true
```

实际认证请求返回 HTTP 200，模型信息位于 `model_catalog`。`/basispoints/api/responses/models` 在本次测试也返回相同的六模型目录；`/basispoints/api/models` 返回 404。目录结论以 OAuth 客户端实际使用的 access 接口为准。

目录公布的思考等级如下；本次实际推理只验证 `low`：

- `gpt-6-astra`：`low`、`medium`、`high`、`xhigh`。
- 其余五个模型：`none`、`low`、`medium`、`high`、`xhigh`。
- 六个模型的 `default_effort` 均为 `medium`，`free_preview` 均为 `false`。

最初的直接探测请求缺少 BPS 的上下文管理与任务元数据，被返回 422；补齐与插件格式一致的字段后，四个额外模型均通过。422 是请求格式问题，不是模型访问被拒绝。

脱敏原始结果保存在本地 `dist/live-models-0.5.18-2026-09-26.log`、`dist/bps-model-catalog-2026-09-26.json` 与 `dist/bps-model-probes-2026-09-26.json`，不随源码包导出。
