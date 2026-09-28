# 原生 Codex ultra / max 实测

状态：历史对比记录。用户最终确定降智检测仅使用原生 gpt-6-astra + XHigh，临时 ultra/max 对比测试入口已移除，不属于正式检测功能。

日期：2026-09-27。使用同一测试账号、其导出文件中关联的代理、gpt-6-astra 及原来的苹果手机问题。仅测试请求直接替换 reasoning.effort；不经过 BPS 请求转换或别名归一化。

| 原生请求参数 | HTTP | 响应回报的 effort | 结果 | 耗时 |
| --- | --- | --- | --- | --- |
| ultra | 400 | 无 | 参数被拒绝，未生成答案 | 1.443 秒 |
| max | 200 | max | iPhone 16 Pro Max | 14.719 秒 |

ultra 的上游错误明确说明：Invalid value: 'ultra'. Supported values are: 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', and 'max'.

因此，不能把本插件 BPS 转换器的 max/ultra → xhigh 规则套用到原生接口。原生接口接受 max，且本次完成响应回报的 reasoning.effort 就是 max。本记录不据此推断内部计算预算或单次回答差异的原因。

max 本次回答按既有固定“17”规则属于疑似降智。问题和规则未修改，单次答案不代表稳定的模型能力判断。

本次实验未修改正式原生检测的 xhigh 配置，也未重建或替换此前的 0.6.8 签名包。比较测试入口随后已按最终范围移除，普通测试默认跳过真实请求，不保存账号凭据或完整响应。
