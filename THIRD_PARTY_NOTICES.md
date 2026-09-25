# 第三方代码与来源

## 随仓库分发的 Sub2API API 契约

本项目通过 go.mod 的本地 replace 使用 third_party/sub2api 中的最小 API 契约副本，无需下载完整的宿主参考工程。

- 上游项目：Wei-Shaw/sub2api
- 来源仓库：https://github.com/Wei-Shaw/sub2api
- 对应提交：a3eb7ef302961cba716dc78b39b93b60c467db0e
- 对应宿主版本：0.2.8
- 上游原目录：backend/pkg/pluginapi/v1/
- 本地目录：third_party/sub2api/pkg/pluginapi/v1/
- 原样复制的文件：plugin.pb.go、plugin_grpc.pb.go、runtime.go、plugin.proto、manifest.schema.json
- 校验记录：2026-09-25 逐项比较，忽略 CRLF/LF 差异后内容一致。
- 上游 LICENSE 标示 GNU Lesser General Public License, Version 3；本仓库保留该提交的完整原文件：[third_party/sub2api/LICENSE](third_party/sub2api/LICENSE)。

third_party/sub2api/go.mod 是用于编译上述副本的本地模块入口。此声明记录来源，不更改第三方许可或上游版权。

## Go 模块依赖

其余模块的精确版本与校验值由 go.mod、go.sum 记录，源码由 Go 模块机制管理，各依赖继续适用其原许可证。发布者应保留对应依赖要求的声明。

## 历史图片中转设计参考

0.5.14 / 0.5.15 的自托管图片中转采用独立插件实现，设计参考 ranxi2001/sub2api v2.8.13 的临时 HTTPS 图片中转、配额与过期机制（backend/internal/service/basispoints/image_relay.go 和 docs/excel-bps.md）。未把该分叉的宿主服务或设置页作为本插件运行依赖。参考版本：https://github.com/ranxi2001/sub2api/tree/v2.8.13 。PNG/JPEG/GIF 解码使用 Go 标准库，WebP 解码使用 golang.org/x/image，其版本与校验值见 go.mod/go.sum。

## 原生附件协议参考

0.5.16 改用 BPS 原生附件上传，不再使用上述自托管下载路径。协议参考 JaxsonWang/cpa-plugin-oai-basispoints 的 internal/basispoints/attachments.go，固定提交 80027a0db6a56c0f88a54a04308dbc38678f0f97：

https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/80027a0db6a56c0f88a54a04308dbc38678f0f97/internal/basispoints/attachments.go

参考点是同源同目录 attachments 端点、multipart 的 file 字段、当前账号认证请求头，以及 openai_file_id 返回值。实现与验证范围见 [internal/attachments/README.md](internal/attachments/README.md)。本地模拟上游和未认证路由检查不代表已完成真实账号上传或模型识图验收。PNG/JPEG/GIF 仍使用 Go 标准库，WebP 使用 golang.org/x/image。

## 本项目自有代码

仓库尚未指定自有代码的统一许可证。本次整理未替权利人选择或授予 MIT、Apache 等许可证；第三方许可不表示对所有自有代码的统一授权。公开发布时请由有权授权者确定并补充项目 LICENSE。
