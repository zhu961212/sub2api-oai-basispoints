# 0.5.26：官方宿主对照审查、自检修复与签名发行

本次以本地官方 Sub2API 0.2.8 源码为对照，提交 a3eb7ef302961cba716dc78b39b93b60c467db0e。发布对象是 local.oai-basispoints 插件；宿主工作区已有改动未被本次修改或上传。验证日期为 2026-09-26 UTC（北京时间 2026-09-27）。

## 功能与修复

- 宿主契约：核对 vendored protobuf/gRPC、插件握手、反向 HostService、配置 Apply/回滚、账号目录、UI Bridge、签名安装与版本兼容。新增真实编译子进程的集成测试，覆盖 GetInfo、Health、broker、配置和多帧转发。
- BPS 账号状态：HTTP 401/403/429、HTTP 200 中的 JSON/SSE 失败、附件与工具纠正路径统一隔离服务级错误及配额头，避免写入宿主原生 Codex 账号状态或无意义换号；原生透传行为保留。
- 403 停用：一次明确 403 停用当前账号的 BPS，使用宿主隔离 KV 存储记录，支持重启恢复、失败重试与明确重新勾选保存后恢复。持久化等待锁也遵守 deadline，取消后不再发起已过期的写入，保留 dirty 状态供后台重试。
- 工具协议：严格解析信封和 function 参数中的重复键，保留大整数，拒绝尾随 JSON；修复可安全恢复的双重编码和截断信封，纠正过程不执行不完整或有歧义的调用。
- 配置界面：修复旧状态轮询覆盖刚发生的 403、普通保存回读遗漏检测触发标记、顶层 null 配置静默恢复默认值。增加单账号检测，保留宿主可调度性判断与完整账号 ID。
- 性能：账号选择与目录避免重复分配和 DOM 重建；检测使用有界 worker，响应读取保持增量、字节限制与取消语义。历史性能报告仅描述当时的本地基准，不代表真实模型速度。
- 打包：只包含当前 manifest 哈希表声明的文件，清除源清单中的旧平台声明，防止复用构建目录时混入旧二进制；拒绝尾随清单和符号链接 UI 文件。
- 验签：Windows/Bash 均验证实际输出路径及配套仓库外公钥，发布者 key_id 必须匹配。独立校验器拒绝重复 ZIP、危险路径、符号链接、非法 schema、缺签和非规范 Ed25519 点；哈希按块读取。

保留已有六模型选择、新账号接入/手动排除、原图和工具截图语义、5xx 有限重试、总超时及 BPS 429 隔离规则。

## 验证清单

发布前执行以下检查，并以本次实际结果为准：

1. 完整离线 Go 回归、go vet、UI JavaScript 语法与全部 Node 回归。
2. 真实 Chrome 的 opaque-origin iframe / sandbox / CSP 下保存与重新打开回归。
3. Python 源码导出、验包信任边界及构建脚本回归（24 项），验签正向夹具由 Go 标准库独立生成。
4. 官方宿主插件、schema、兼容、安全、broker 与真实账号错误分类器测试；使用 overlay 加测试，不改变宿主源码。
5. Windows/Linux amd64 重新编译，原发布公钥独立 Ed25519 验签，官方安装器安装并启动实际签名包。
6. 推送后检查 GitHub Actions 的 Linux 回归、race 与跨平台构建，成功后公开 Release。
7. 核对 GitHub 上传摘要和公开下载内容，与本地已验签产物逐字节比较。

本机默认 CGO_ENABLED=0 且没有可用 C 编译器，因此本地不声称通过 race；该项交由仓库 Linux CI 实际执行。

## 本地实际结果

- 完整 Go 回归及 go vet 通过，205 项 Node UI 回归通过，Chrome 沙箱保存/重开通过。
- Python 23 项通过、1 项原生 Bash mock 用例在 Windows 跳过；真实 Git Bash 另用方括号/空格输出路径、带空格 key_id 和已有 Linux 构建目录执行签名包烟测通过。
- 官方宿主 API/schema、插件与账号隔离回归通过；官方安装器对 0.5.26 签名候选执行受信验签、安装、启动、broker、配置/Health 与 125KB 多帧转发通过。
- 403 账号持久化取消、deadline、重绑定及旧在途写入回归连续 20 轮通过。
- 正式发布前从干净提交再次构建并验签；最终包哈希、GitHub CI 状态与下载复核结果记录于 GitHub Release 正文和附件。

## 产物与升级

发布版本 0.5.26；签名身份沿用 oai-basispoints-v1，公钥与上一版 v0.5.25 发布附件一致，已有 trusted_publishers 配置无需换钥。

- local.oai-basispoints-0.5.26.s2plugin
- local.oai-basispoints-0.5.26.s2plugin.sha256
- publisher.public
- sub2api-oai-basispoints-0.5.26-github-source.zip
- sub2api-oai-basispoints-0.5.26-github-source.zip.sha256

实际哈希以 Release 中的 SHA-256 附件及资产摘要为准；私钥、账号凭据、宿主本地配置和构建缓存不进入仓库、源码 ZIP 或 Release。源码与二进制从同一已审核版本生成。

安装签名包后重新加载插件，并确认运行版本为 0.5.26。真实 OAuth/BPS 上游会话与生产部署不在本次离线验证范围内；没有自动部署到运行中的服务器，也不能承诺第三方上游永远不返回错误。
