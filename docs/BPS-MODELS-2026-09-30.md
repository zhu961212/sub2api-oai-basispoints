# BPS 模型目录核验：2026-09-30

## 结论与范围

北京时间 2026-09-30 21:31–21:32，通过先前已授权的单个 OpenAI OAuth
账号及其关联代理，实时查询了 BPS 官方模型目录。**该账号本次可见的目录仍为
6 个模型；与 2026-09-26 的留存清单相比，新增 0 个、删除 0 个。**

这是一份账号可见目录的核验结果，不代表所有账号、地区、计划或灰度范围的
全局模型列表。本轮没有发送生成请求，未重测六模型的实际推理能力，也没有
改动插件模型选择、账号状态或服务部署。

## 接口与时间

官方服务源为 https://bps.openai.com；历史前端调用来源见
[live-models-2026-09-26.md](live-models-2026-09-26.md)。以 OAuth 客户端使用的
access 接口为主，responses/models 为交叉核对。时间为本地收到响应后的记录
时间（UTC+08:00），不是请求开始时间。

| 本地响应记录时间 | GET 路径 | HTTP | 用途 |
| --- | --- | --- | --- |
| 2026-09-30 21:31:12.972 | /basispoints/api/responses/access?include_models=true | 200 | 账号访问状态与模型目录 |
| 2026-09-30 21:31:14.623 | /basispoints/api/responses/models | 200 | 独立目录交叉核对 |
| 2026-09-30 21:32:15.685 | /basispoints/api/responses/models | 200 | 补充核对模型对象字段及上下文限制 |

前两次目录规范化后完全一致。access 返回 allowed=true；目录的 source=chatgpt、
default_model=gpt-5.6-sol、restricted_models=[]。第三次查询只补充检查首次脱敏
白名单没有记录的字段名称，六模型目录保持一致。总计 3 次只读 GET，0 次生成
请求；没有进行自动重试或令牌刷新。

## 本次完整可见清单

| 模型 ID | 默认思考等级 | 目录公布的思考等级 | free_preview | 与 9 月 26 日对比 |
| --- | --- | --- | --- | --- |
| gpt-5.6-luna | medium | none、low、medium、high、xhigh | false | 保留，无变化 |
| gpt-5.6-terra | medium | none、low、medium、high、xhigh | false | 保留，无变化 |
| gpt-5.6-sol | medium | none、low、medium、high、xhigh | false | 保留，无变化 |
| gpt-6-astra | medium | low、medium、high、xhigh | false | 保留，无变化 |
| gpt-6-luna | medium | none、low、medium、high、xhigh | false | 保留，无变化 |
| gpt-6-sol | medium | none、low、medium、high、xhigh | false | 保留，无变化 |

六个模型的 description 均为空。和历史脱敏快照相比，ID、标签、描述、默认
思考等级、公布的思考等级、free_preview、目录默认模型、来源及限制清单一致。

**目录没有公布上下文窗口、最大输入 token 或最大输出 token。** 第三次查询
确认目录仅包含 default_model、models、restricted_models、source；每个模型
对象仅包含 default_effort、description、efforts、free_preview、id、label。
因此无法从本接口推导上下文或输出上限；插件请求中的 compaction 阈值不能
当作模型上下文窗口，其他产品中同名模型的参数也不能替代此接口证据。

## 与插件配置、推理能力的区别

- 当前插件源码 0.6.10 的后端 availableModels 和前端 MODEL_IDS 均已包含上述
  六个模型，不需要为本次目录结果增加模型。后端定义位于
  internal/config/config.go，前端定义位于 ui/assets/app.js。
- 插件默认只选择 gpt-6-astra 与 gpt-5.6-sol；六个“可选模型”不等于全部
  已启用，运行服务的实际选择仍取决于已保存配置。本轮未读取或修改线上配置。
- 插件对外默认模型为 gpt-6-astra，官方目录默认模型为 gpt-5.6-sol，二者
  各自表达不同配置范围，并非本轮出现的模型变化。
- 目录的 none 与插件的 reasoning 映射规则应分开理解；本轮没有调整映射。
- 2026-09-26 文档记录过六模型短文本生成成功，但那是历史单账号测试。
  本轮 allowed=true 和 HTTP 200 只能确认本次访问检查和目录读取成功，
  不能证明现在每个模型都可推理、额度充足或各种工具能力均可用。

## 可复查证据

随本文保存的 [BPS-MODELS-2026-09-30.json](BPS-MODELS-2026-09-30.json) 包含
三个请求的 UTC 记录时间、服务端 Date、HTTP 状态、返回字节数，以及允许保留的
模型目录元数据。没有 token、账号标识、账号名称、邮箱或代理地址。

证据 JSON 的 SHA-256：

    9942101777921e260b7e5f182a5b5c1f70fe5ef038e57edf5ce930563820047a

本地忽略目录 build/bps-models-2026-09-30/ 另保留首次和交叉查询的独立脱敏
结果 access-catalog.json、models-catalog.json、models-metadata-catalog.json，
以及本轮只读 probe_readonly.py 和 probe_metadata_readonly.py。探针复用
先前目录脚本的 GET 部分，删除了所有 POST/生成代码，原账号导出文件未修改。
历史对比基线为 dist/bps-model-catalog-2026-09-26.json；它与本轮 build 目录
同属本地证据，不随源码包导出。
