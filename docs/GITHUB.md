# GitHub 上传说明

**交流群：1107265919**

本项目仓库：https://github.com/zhu961212/sub2api-oai-basispoints 。交流群：1107265919。以下步骤也可用于维护自己的 fork；创建新远程仓库时替换为相应地址。

## 已准备的源码包

在项目根目录运行 python -X utf8 tools/export_source.py，生成 dist/sub2api-oai-basispoints-0.5.20-github-source.zip 和配套 SHA-256。脚本从当前工作区导出源码，包含尚未提交的新文件；不会复制 .git、build、dist、发布密钥、常见凭据文件或本地缓存。解压包后，以内层项目目录作为仓库根目录。

README、插件清单介绍、配置页和本文均包含 **交流群：1107265919**；GitHub 仓库 About 或 Release 介绍也可填写：

> 适用于 Sub2API 官方版，通过 OpenAI Excel（Basis Points）官方入口使用账号可用的官方模型，六模型可选。交流群：1107265919。

已提供 .github/workflows/ci.yml：检查 Go、UI、源码导出边界，并在 Linux 上运行 race 与跨平台构建。工作流只有读取仓库权限，不使用发布私钥，不自动部署或发布 Release。本地测试通过不表示 GitHub Actions 已运行；推送后应查看实际工作流结果。

## 1. 确认上传范围

仓库根目录应是 **sub2api-oai-basispoints 项目文件夹本身**，不要把外层工作目录、参考工程或本地归档一起上传。GitHub 首页应直接看到 README.md、go.mod 和 manifest.source.json。

保留源码、测试、构建脚本、配置 UI、文档，以及 go.mod 通过 replace 引用的 third_party/sub2api 契约文件。

以下内容不进入源码仓库：

| 内容 | 处理方式 |
| --- | --- |
| build/、dist/、运行时二进制、.s2plugin | 本地生成；安装包使用 GitHub Releases 附件发布 |
| 发布私钥、账号导出、Token、Cookie、实际宿主配置、.env | 保存在仓库外，不上传 |
| 临时探针、调试输出、浏览器配置、缓存、备份 | 清理或保存在仓库外的本地归档 |
| 参考工程和第三方完整 checkout | 不随本项目上传；保留明确依赖的契约副本 |

.gitignore 能防止常见生成文件被意外加入 Git，但不能替代提交前检查，也不会自动移除已被跟踪的敏感文件。不要使用 git add -f 绕过排除规则。

## 2. 检查源码与测试

在项目根目录运行：

~~~powershell
go test ./... -skip TestLive -count=1 -timeout 120s
go vet ./...
node --test tools/ui.test.cjs
node tools/test-ui-browser.mjs
~~~

最后一项需要 Chrome / Edge / Chromium。Go 测试命令明确跳过真实账号测试，即使本机已有实测环境变量，也不会请求真实上游。首次执行仍可能下载 Go 模块与工具链。环境要求和验包步骤见 [README](../README.md)。

检查待上传内容：

~~~powershell
git status --short --untracked-files=all
git ls-files
git diff --cached --stat
git diff --cached --check
~~~

尚未执行 git add 时，已跟踪文件列表和暂存区检查可能为空；加入文件后再检查一次。重点确认新增文件属于项目，不包含真实 Token、账号导出、签名私钥或本机敏感配置。

## 3. 确认许可与来源

公开前确认以下两项：

1. **自有代码许可**：当前未设置项目许可证。由权利人决定采用何种许可，再添加相应 LICENSE；不要因上传 GitHub 自动标注 MIT。
2. **第三方来源**：核验复制文件的来源、版本和再分发要求，保留适用版权与许可文件。参见 [第三方说明](../THIRD_PARTY_NOTICES.md)，Go 模块依赖仍适用各自许可。

许可尚未明确时，可先准备本地提交或私有仓库，待确认后再公开。本说明不授予任何未获得授权的第三方权利。

## 4. 建立本地提交

如目录已初始化 Git，跳过初始化命令，先用 git status 确认当前状态：

~~~powershell
git init -b main
git status
~~~

确认 .gitignore 生效后加入项目文件，再审阅暂存区：

~~~powershell
git add .
git diff --cached --stat
git diff --cached --check
git diff --cached
~~~

不要在公开渠道粘贴包含凭据的差异输出。确认待提交文件正确后创建提交：

~~~powershell
git commit -m "Prepare Basis Points plugin source for GitHub"
~~~

提交作者由本机 Git 配置决定。没有配置时，请设置自己的 user.name 和 user.email，不要使用他人身份或项目内示例值。

## 5. 创建远程仓库并推送

在 GitHub 新建空仓库。已有本地 README 时，创建页面不要再次生成 README、.gitignore 或许可证，避免不必要的初始历史冲突。

将下方地址替换为真实地址后执行：

~~~powershell
git remote add origin https://github.com/zhu961212/sub2api-oai-basispoints.git
git remote -v
git push -u origin main
~~~

如果 origin 已存在，先核对其地址，不要盲目新增或覆盖。通过 GitHub 支持的凭据管理器、SSH 或 GitHub CLI 完成认证，不要把访问令牌写进远程 URL、脚本或文档。

## 6. 发布安装包

源码上传与安装包发布分开进行。需要发布 0.5.20 时：

1. 确认源码、manifest.source.json 版本、测试结果和发布记录一致。
2. 按 [README](../README.md) 生成签名安装包，用配套公钥独立验包。
3. 在 GitHub Releases 为审核后的提交创建 v0.5.20 标签和 Release。
4. 上传 .s2plugin 与配套 .s2plugin.sha256，正文参考 [0.5.20 发布记录](release-0.5.20-2026-09-26.md)。
5. 向部署者提供 key_id 和用于验签的发布公钥；**绝不发布私钥**。

0.5.20 的发布正文应说明六模型按 GPT-6 与 GPT-5.6 分两排显示，默认只启用 `gpt-6-astra` 和 `gpt-5.6-sol`；只有已选且命中灰度、符合账号白名单的模型转发到 Basis Points，上游保留请求模型名。明确区分空模型选择（全部透传）与空账号选择（不限制账号）；未选模型和旧别名原样透传。如实记录本次本地测试、构建和验签结果，未完成的真实上游验证不得写成已通过。正式发布复用原发布密钥，key_id 为 `oai-basispoints-v1`，并用配套公钥独立验签。不要用旧自托管路径的性能数字或 0.5.15 安装包哈希充当本次结果。

历史发布记录中的哈希对应当次构建。重新构建后的产物应重新计算、核验，并更新实际发布附件的校验信息；不能直接套用历史哈希。

源码仓库中不需要保留 dist/。已有安装包若已移出项目，可从本地归档取回作为发布候选，验包后再上传为 Release 附件。

## 7. 上传完成后核对

检查 GitHub 首页是否正确显示 README，文档链接是否可打开，文件树中是否只包含预期项目文件，Release 下载附件是否与发布哈希一致。

若秘密已被上传，仅删除当前文件不足以清除 Git 历史；应立即撤销或轮换受影响凭据，再按 GitHub 的敏感数据移除流程处理历史。未泄露的本地归档无需上传。
