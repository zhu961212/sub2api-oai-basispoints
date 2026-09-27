# 0.6.4：补齐 #40874 的 HTTP 工具失败路径

日期：2026-09-27（UTC+8）。插件 ID 为 local.oai-basispoints，沿用
oai-basispoints-v1 和原 Ed25519 发布公钥，适用 Sub2API >=0.2.8 <0.3.0。

## 已复现的问题

用已发布的 0.6.3 签名包内 Windows 进程，回放 #40874 的原始失败事件，
再交给原版 0.2.8 宿主处理：HTTP 200 SSE 路径通过；上游 HTTP 502 路径
仍被宿主 HTTP 错误分支替换为 Upstream service temporarily unavailable。
仅补 error.type 无法修复这条分支，改成 HTTP 400 也仍会丢掉具体诊断。
原版宿主基准为 a3eb7ef302961cba716dc78b39b93b60c467db0e。

## 修复行为

- 只在 BPS 的 HTTP 错误响应中有界识别确定的 invalid_tool_call；最多
  探测 64 KiB，不扫描普通输出中的关键字。未知、冲突或超限内容恢复原字节。
- 确定失败后停止重复 POST，并清空失败响应中的可调用工具输出；不重新
  执行代码、不换账号、不把失败变成 completed。
- 以成功的 HTTP 传输携带明确的 failed Responses 对象，让宿主走正确的
  终态分支。客户端请求流式时发送 response.failed SSE，非流式时发送
  failed JSON，保留 invalid_tool_call 与安全诊断；转换开关关闭也生效。
- SSE 识别到失败终态后关闭原响应，不等待上游 EOF。非匹配错误保留已有
  HTTP 状态和重试策略；401/403/429、语义账户限制及未选模型透传保持既有行为。
- 仅在既有策略允许下一次 HTTP 重试时，把错误探测等待限制为一秒；
  禁止重试与最后一次尝试遵循原请求期限，避免截断普通慢错误正文。

## C 代码与验证范围

继承 0.6.3 的合法源码字节保真和最多一次受限纠正。额外复测 12 种常见
C 语法、两种字段顺序与四层序列化，共 96 个合法输入检查均通过；24 个
损坏输入均未执行坏源码，其中 16 个允许一次纠正。字符串边界仍有歧义
的损坏调用继续拒绝，不以猜测源码的方式放宽。

新增回归覆盖 HTTP/SSE/JSON、客户端流式和非流式、转换开关、错误字段
位置、确定失败只请求一次、普通 502 保真和有界预读。回放程序直接使用
发行包中的真实插件进程和原版宿主 Go overlay，不修改宿主源码。

日志未包含原始工具参数，因此不能据此还原当时的 C 源码，也不宣称所有
歧义输入都能修复。失败终态本身没有可安全纠正的原始调用，保留其失败。
本次验证使用合成账号与本地上游，不运行 TestLive，也不代表已替换生产
服务器。完整构建、Linux CI、签名及公开附件哈希以本次 Release 为准。

可复测最终签名包的原版宿主链路（宿主源码和 Go 依赖须已存在）：

```powershell
python -X utf8 tools/host-relay-integration/run_signed.py --host-source <Sub2API源码目录> --package dist/local.oai-basispoints-0.6.4.s2plugin --public-key dist/publisher.public --proof build/relay-host-proof.json
```

脚本先强制校验包签名，再从包中取出本机运行时，经真实插件 RPC 接入
宿主流读取逻辑；Go overlay 只增加测试，不落地或修改宿主源码。

## 安装

1. 停用并卸载旧插件；旧插件配置随卸载删除。
2. 上传 local.oai-basispoints-0.6.4.s2plugin 签名包。
3. 重新填写模型、账号、403 开关、绑定及灰度配置，然后启用。
4. 重新打开配置页，确认实际运行版本为 0.6.4。

不兼容或迁移旧版配置；已信任的原公钥无需更换。0.6.1 继续保持撤回。
