# BPS 使用政策 403：可确认的规则与诊断边界

核对时间：2026-09-26 UTC（北京时间 2026-09-27）。以下页面已使用系统 HTTP 客户端直接从 OpenAI 官方站点获取并核对正文；不是依据搜索摘要或论坛推测。

## 官方规则

1. [OpenAI Terms of Use](https://openai.com/policies/terms-of-use/)（生效：2026-01-01）适用于个人服务，包含不共享账号凭据或使账号供他人使用、不自动或程序化提取数据/输出、不出售/出租/分发服务、不得绕过速率限制及保护措施等要求。同页明确 API、Enterprise 和其他开发者/商业服务另由 Business Terms 管理，不能把个人服务限制机械套用到所有正规 API 调用。
2. [OpenAI Usage Policies](https://openai.com/policies/usage-policies/)（生效：2025-10-29）包含恶意网络活动、诈骗、侵害隐私、未成年人剥削及绕过安全措施等禁止用途；违反或规避规则可能导致失去访问权限。
3. [Why was my OpenAI account deactivated?](https://help.openai.com/en/articles/10562188-why-was-my-openai-account-deactivated) 说明内容/用途违规、规避安全或访问限制、不当共享账号或 API key、影响服务完整性，以及疑似凭据被盗的异常活动可能导致限制；安全原因可能是临时保护性停用。该说明不是每个 BPS 403 都已停用整个账号的证据。
4. [Service Terms](https://openai.com/policies/service-terms/)（更新：2026-09-21）的 API 条款要求按适用 API 文档使用；不能因此认定未公开的 BPS 接口属于有通用 API 使用授权的公开 API。

以上是公开的一般规则。没有取得 BPS 专属的命中规则、调用次数/IP/设备阈值或固定解禁时长，也没有证据表明仅“使用代理”或“某个并发数”就必然封禁。

## 与本次账号的关系

访问检查 GET /basispoints/api/responses/access 返回 HTTP 200、allowed=true，但两次使用插件原生构造的 gpt-6-astra / low / Say OK 实际推理均为 HTTP 403。错误为 “403: This request was blocked by our usage policy.”，type=server_error、code=null，且没有 Retry-After 或已核对的额度重置时间头。

这证明上游按使用政策拒绝了这类实际请求，不足以证明具体违反哪条规则、整个 OpenAI 账号已被封禁，或多久恢复。简单 Say OK 也失败，使账号授权、客户端/访问方式或风险控制值得优先核查；这是排查方向，不是已确认的命中原因。

对将个人 OAuth 凭据接入服务端转发的部署，应核对账号是否被多人共享、是否在不适用的服务入口进行程序化提取，以及是否规避已生效的访问限制。不能仅凭项目使用了 OAuth 或转发架构，就认定实际已命中上述某条政策。

若需确定账号级具体原因，应检查 OpenAI 发出的账号通知，并通过官方支持或通知中的申诉入口查询；现有响应无法给出这一内部判定。插件本地 403 停用是另一个无自动到期的记录，见 [停用与期限说明](BPS-403-ACCOUNT-DISABLE.md)。
