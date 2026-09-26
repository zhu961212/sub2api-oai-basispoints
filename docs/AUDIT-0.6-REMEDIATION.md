# 0.6.0 审查修复与验收

日期：2026-09-27（北京时间）；基线：f6049724102592d977305234476e17d6f92aad1f。修复对应 [原审查报告](AUDIT-0.6-PREPARATION.md)。本次完成源码修复和离线验收，发行版本为 0.6.0。用户已授权打包、签名和上传；最终安装包哈希、独立验签和发布状态见 [0.6.0 发布记录](release-0.6.0-2026-09-27.md)，未完成的发行检查在其中明确标注。

## 处理结果

| 审查项 | 结果 | 核心行为 |
| --- | --- | --- |
| 1. 流式失败仍释放工具 | 已修复 | 实际 SSE event、正文 type、两层 error/status 共用终态判断；失败优先，清空暂存工具，禁止修复合并和成功上下文缓存。 |
| 2. JSON 转 SSE 伪造成功 | 已修复 | failed/incomplete/cancelled 不再变成 completed；失败输出移除可调用工具；取消保留 upstream_cancelled 分类。 |
| 3. 缓冲 SSE 丢失败分类 | 已修复 | 锁定首个终态；失败后不能被追加 completed 覆盖；保留 bps_service_rejected；空、非 JSON、仅事件名或 DONE 不能绕过真实失败。 |
| 4. 多页面检测错账号 | 已安全降级 | 官方 0.2.8 Bridge 无请求级目标接口；删除共享 save/test 检测链，暂停主动单账号与批量检测；后端拒绝遗留触发。 |
| 5. 残留公钥破坏未签名构建 | 已修复 | 只有明确签名构建才自动附加配套公钥；正式签名、key ID 和独立验签要求保留。 |

第 4 项没有宣称恢复主动检测功能。普通 TCP/TLS 连通性测试、被动状态、配置保存、403 自动停用和明确恢复仍可用。要保留主动检测，宿主需提供原子绑定目标和配置快照的接口，详见 [检测接口兼容说明](ACCOUNT-CHECK-BRIDGE.md)。本次未修改宿主。

独立复核另发现并修复两个边界：失败 wire event 的空数据/非 JSON/DONE 曾被提前忽略，以及合成 created/in_progress 事件携带最终 error 导致误判。正式回归已覆盖这些情况，未仅依赖忽略目录中的复现。

## 验收证据

- go test ./... -skip TestLive -count=1 -timeout 120s：全部通过。
- go vet ./...：通过。
- node --test tools/ui.test.cjs：218/218 通过，0 跳过。主动检测旧事务测试随该能力暂停替换；底层隔离/凭据/代理测试保留，并新增双页面和旧页面零派发覆盖。
- python -X utf8 -m unittest discover -s tools -p test_*.py -v：发行前补充 Windows PowerShell 5.1 编码兼容回归后，31/31 通过、0 跳过。Windows PowerShell 5.1、PowerShell 7 和 Git Bash 均运行实际包装器与真实验签器矩阵。
- 原审查协议 overlay 的全部 TestAudit：通过，原有 7 组问题不再复现。
- 原残留公钥构建复现：有/无残留公钥均退出 0。
- git diff --check：通过；当前修改和新增文件未发现用户提供的真实 access/id/refresh token。

本地 build 目录中的审查与账号探测 Go 文件属于覆盖测试夹具，已通过该忽略目录内的独立 go.mod 与生产模块隔离；原 overlay 命令仍可执行。没有通过跳过生产包来获得全库测试通过。本机未发现可用的 C 编译器，本轮未运行 Go race；仓库 Linux CI 已配置 race 检查。

详细日志保存在本地忽略目录 build/audit-0.6-go.log、audit-0.6-vet.log、audit-0.6-ui.log、audit-0.6-python.log 和 audit-0.6-original-repro.log。

## 用户账号 403 实测

用户提供的账号访问检查返回 allowed=true；用户随后明确授权真实推理测试，两次 gpt-6-astra / low / Say OK 请求均被上游以使用政策原因拒绝（HTTP 403）。正文与时间相关响应头均未给出禁用期限。详情见 [403 期限查询与实测](BPS-403-ACCOUNT-DISABLE.md)。这项真实测试失败是上游访问结果，不是离线回归失败；未通过改写失败或关闭保护掩盖它。
