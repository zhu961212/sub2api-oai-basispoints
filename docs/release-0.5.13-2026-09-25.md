# 0.5.13 本地签名发布记录

日期：2026-09-25

本文记录当次本地构建。源码整理后，安装包、构建日志及发布密钥已移到工作区外归档，不随 Git 上传；下方命令保留当时的相对路径，新构建请使用 README 中的仓库外密钥示例。

## 修复内容

- 账号区增加“全选列表账号”按钮，一次补选当前列表中的账号，保留暂未出现在列表中的已保存账号。点击后仍需保存；加载、保存和回读确认期间禁止操作，空列表或已全选时按钮禁用。
- 渲染前过滤无效账号 ID 并去重，修复重复复选框造成的勾选状态矛盾；没有有效账号时显示明确提示。保存失败后保留当前选择并恢复操作。
- 移除传输提示词写死的 exec_command 示例，使用当前请求实际声明的完整工具名。执行器描述中的内部 helper 只能通过已声明的执行器调用，不能作为中继目标。
- 明确 custom 工具的原始文本放在传输信封 code.args，插件最终还原为 custom_tool_call.input；前置目录和末尾提醒采用一致表述。
- 真正目录外、歧义或参数非法的调用仍以 invalid_tool_call / response.failed 拒绝，随后输出一个 [DONE] 和结束帧。客户端可能仍将这种失败终态显示为 stream disconnected；本次修复消除误导调用的提示词，不把非法工具伪装成成功。

## 本地验证

- 全量 Go 本地测试通过：go test ./... -skip TestLive -count=1 -timeout 120s。明确跳过真实上游测试。
- go vet ./... 通过。
- 工具目录聚焦回归通过：executor-only 目录、custom 原始文本与历史回放、嵌套 helper 误调用拒绝、namespace 解析、目录替换和撤销、流式失败收尾。
- UI 单元回归 39/39 通过；新增重复账号、全部账号无效、保存失败后恢复操作的覆盖。
- 真实 Chrome 沙箱回归通过：sandbox allow-scripts、CSP form-action none；两次保存和五次读取，全选与清空保存后销毁页面重开均保持一致。使用本地模拟宿主。
- Python 独立校验器重新核对包内每个文件的哈希，并验证 manifest 原始字节的 Ed25519 签名，结果通过。

## 构建与产物

- 版本：0.5.13
- 平台：windows/amd64、linux/amd64
- 签名 key_id：oai-basispoints-v1；使用现有发布密钥
- 安装包：dist/local.oai-basispoints-0.5.13.s2plugin
- 大小：12650432 字节
- SHA256：b728ceba28203a3e661ee21161529e04a04bafac8647b40d57d55c96c5531b42
- 同目录生成 .s2plugin.sha256 校验文件
- 包内共 8 项：清单、签名、两个运行时和四个 UI 文件

~~~powershell
go run ./tools/packager -targets windows/amd64,linux/amd64 -signing-key build/keys/publisher.private -key-id oai-basispoints-v1
python -X utf8 tools/verify_package.py dist/local.oai-basispoints-0.5.13.s2plugin --public-key build/keys/publisher.public
~~~

## 更新与验证范围

本次完成本地代码、回归和签名安装包，尚未部署到宿主，也未发起真实账号请求。将安装包上传到 Sub2API 插件管理页更新并重载插件，确认运行版本为 0.5.13，再重新发起原失败请求。仍在运行的旧插件不会因本地源码或包文件变化而自动使用新提示词。

已保持原有路由：仅符合账号白名单的 gpt-6-astra 请求走 Basis Points；其他模型原样透传。空账号选择仍表示不限制账号。
