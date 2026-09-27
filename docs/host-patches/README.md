# Sub2API 请求快照检测补丁（配套 BPS 0.6.1）

该附件是本项目配套的宿主源码补丁，不是官方原版 Sub2API 发布包，也不表示已部署到任何运行中的宿主。仅升级 BPS 插件不足以恢复主动检测：宿主后端和管理前端也必须应用本补丁并重新构建。

## 基准与内容

- 宿主基准版本：本地官方 Sub2API 0.2.8 源码。
- 精确基准提交：`a3eb7ef302961cba716dc78b39b93b60c467db0e`。
- 补丁文件：`sub2api-0.2.8-config-test-scoped.patch`。
- 补丁 SHA-256：`ba559b08e31e70db5a63a396b864de62fc46071476b2085f5f7c59012b2aa31f`。
- 变更范围：12 个文件，7 个已有文件修改、5 个新增文件；不包含工作区原有的依赖锁文件、账号页面或其他改动。
- 附件只含源码补丁、说明和校验摘要，不含宿主二进制、账号配置、凭据或私钥。

此补丁新增经过管理员认证和二次验证的 `POST /admin/plugins/:id/test-scoped`，使用请求自己的配置快照和请求 ID 调用插件。检测不会保存临时字段或应用表单配置，也不会因另一页面保存而替换目标。宿主 UI 会话通过 `capabilities: ["config.testScoped"]` 声明真实支持，管理前端把能力及请求快照转发给插件 UI。旧版普通配置测试保持原行为。

请求从入口起共享 30 秒总期限。不同检测可以并行，检测不占用保存或停用所需的全局操作锁；运行时被停用或替换时不换用新客户端重发。相同插件、相同请求 ID 的重复请求在同一宿主进程内去重，结果与不确定失败保存 10 分钟，缓存最多 256 条。此保护不跨进程重启、多实例或缓存期限。新的点击使用新的请求 ID，会发送新的实际检测请求。

## 应用补丁

在对应基准的干净宿主工作副本中执行，`/path/to/` 替换为解压附件的位置。先检查成功再应用；如不是该提交，应先审查移植差异。

```sh
git rev-parse HEAD
# 应为 a3eb7ef302961cba716dc78b39b93b60c467db0e
git apply --check /path/to/sub2api-0.2.8-config-test-scoped.patch
git apply /path/to/sub2api-0.2.8-config-test-scoped.patch
git diff --stat
git status --short
```

## 检查与构建

使用宿主源码要求的 Go 1.27 工具链以及该基准 Dockerfile 固定的 pnpm 9。前端依赖按宿主锁文件安装，不需把本次开发工作区的锁文件修改带入补丁。

```sh
corepack enable
corepack prepare pnpm@9 --activate
cd frontend
pnpm install --frozen-lockfile
pnpm exec vitest run src/api/__tests__/admin.plugins.spec.ts src/views/admin/__tests__/PluginsView.spec.ts
pnpm run typecheck
pnpm run build
cd ../backend
go test ./internal/service ./internal/handler/admin ./internal/server/routes -run TestPlugin -count=1
go build -tags embed -o sub2api ./cmd/server
```

前端构建产物由宿主构建配置输出到 `backend/internal/web/dist/`；`-tags embed` 将更新后的管理前端嵌入后端二进制。也可使用补丁后的宿主 Dockerfile 构建镜像。不要只替换前端或只替换后端。此补丁不包含数据库迁移。

## 升级顺序

1. 应用补丁、完成检查，并构建包含前后端的配套宿主二进制或镜像。
2. 按现有部署方式升级并重启宿主；多实例部署应统一升级，避免页面获得的能力与实际处理请求的实例不一致。
3. 通过宿主插件管理页安装已签名的 BPS 0.6.1 插件包，启用插件。
4. 关闭旧配置页后重新打开。新版宿主会返回 `config.testScoped` 能力，插件据此启用单账号和列表主动检测。旧宿主保持安全禁用。
5. 全选只更新当前表单选择，保存后生效；单账号检测不自动保存配置；一键检测结果产生的选择也需明确保存。检测会发送真实上游请求，可能消耗账号额度。

## 文件清单

```text
backend/internal/handler/admin/plugin_handler.go
backend/internal/handler/admin/plugin_scoped_diagnostics_test.go
backend/internal/server/routes/admin.go
backend/internal/server/routes/plugin_scoped_diagnostics_test.go
backend/internal/service/plugin_manager.go
backend/internal/service/plugin_scoped_diagnostics.go
backend/internal/service/plugin_scoped_diagnostics_test.go
backend/pkg/pluginapi/docs/ui-bridge.md
frontend/src/api/admin/plugins.ts
frontend/src/api/__tests__/admin.plugins.spec.ts
frontend/src/views/admin/PluginsView.vue
frontend/src/views/admin/__tests__/PluginsView.spec.ts
```

## 发布前验证

2026-09-27，从上述提交单独导出干净源码快照，验证 `git apply --check` 后实际应用补丁。12 个目标文件与本次预期工作区文件逐字节一致，并核对仅含指定的 12 个变更文件。没有使用其他未提交改动作为补丁依赖。

在该源码快照上执行 `go test ./internal/service ./internal/handler/admin ./internal/server/routes -run TestPlugin -count=1`，三个后端包均通过。前端复用本机已安装的工具依赖：两个目标测试文件共 24 个测试通过，`vue-tsc --noEmit` 通过。以上不是完整生产镜像构建或线上部署验证；测试使用本地模拟，不调用真实账号。

ZIP 中的 `SHA256SUMS` 用于核对补丁和本说明；ZIP 外的同名 `.sha256` 文件用于核对完整附件。
