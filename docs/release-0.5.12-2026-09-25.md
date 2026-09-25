# 0.5.12 本地签名发布记录

日期：2026-09-25

## 路由验证

顶层小写 model 字段去掉首尾空白后，仅精确的 gpt-6-astra 进入 Basis Points，并继续遵循账号白名单。其他模型透传到宿主原上游。大小写变体、旧别名、未知模型、空值与异常字段均不能启用 BPS 转发。旧 models、model_map、upstream_model 配置无法扩大接管范围。

本地测试新增 52 个透传场景（26 种输入 × HTTP 200/429），校验 URL/查询参数、PATCH 方法、请求和响应字节、凭据及多值业务头保持一致，BPS 命中数为零。另覆盖 Astra 正例、首尾空白兼容及白名单分流。现有 hop-by-hop/传输层头过滤和账号 ID 0 白名单兼容行为保持原样。

全部 Go 测试、33 项 UI 测试及 go vet 通过；真实上游测试未运行。

## 构建与签名

- 插件版本：0.5.12
- 平台：windows/amd64、linux/amd64
- 签名算法：Ed25519
- key_id：oai-basispoints-v1
- 使用已有 publisher 密钥；公钥 build/keys/publisher.public
- Python 独立验证器重新校验全部文件哈希与 manifest 原始字节签名，结果通过
- 包内仅包含清单、签名、两个运行时和四个 UI 文件
- 本次生成本地安装包，尚未部署到宿主

~~~powershell
./build.ps1 -Targets 'windows/amd64,linux/amd64' -SigningKey 'build/keys/publisher.private' -KeyId 'oai-basispoints-v1'
python tools/verify_package.py dist/local.oai-basispoints-0.5.12.s2plugin --public-key build/keys/publisher.public
~~~

## 产物

当时构建产物：dist/local.oai-basispoints-0.5.12.s2plugin。历史二进制与密钥不随源码分发；下方命令和输出保留当时的相对路径。

大小：12674738 字节

SHA256：8c873184a06216f611729818c38a097c57b5280151d3d28fd2da6cc5c79df939

## 构建与验签输出

~~~text
==> go test ./... -count=1
?   	github.com/wangyunjeff/sub2api-oai-basispoints/cmd/oai-basispoints	[no test files]
ok  	github.com/wangyunjeff/sub2api-oai-basispoints/internal/auth	0.351s
ok  	github.com/wangyunjeff/sub2api-oai-basispoints/internal/config	0.364s
ok  	github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol	0.853s
ok  	github.com/wangyunjeff/sub2api-oai-basispoints/internal/transport	1.400s
?   	github.com/wangyunjeff/sub2api-oai-basispoints/tools/keygen	[no test files]
?   	github.com/wangyunjeff/sub2api-oai-basispoints/tools/packager	[no test files]
==> node --check ui/assets/*.js
==> node --test tools/ui.test.cjs
✔ configuration actions work without sandboxed form submission (1.9329ms)
✔ selected accounts save on click, persist on read-back, and survive reopening (7.7155ms)
✔ saving an empty selection persists an explicit account_ids array (2.3037ms)
✔ a host-normalized null account_ids confirms an empty selection and survives reopening (2.0166ms)
✔ a delayed initial load cannot overwrite an allowed user edit (1.1925ms)
✔ failed initial reads keep controls locked until a successful retry (1.173ms)
✔ the page stays locked until save read-back completes (1.1779ms)
✔ a save acknowledgement with unchanged persisted accounts never reports success (1.0776ms)
✔ a failed verification read never turns a save acknowledgement into success (1.24ms)
▶ legacy account_id migrates only when account_ids is absent
  ✔ legacy single account (1.6018ms)
  ✔ explicit empty selection wins (1.1712ms)
  ✔ explicit selected accounts win (1.7427ms)
✔ legacy account_id migrates only when account_ids is absent (5.2634ms)
✔ bridge accepts confirmed config objects and clears the request timeout (1.371ms)
▶ bridge rejects missing or invalid config rather than fabricating success
  ✔ loadConfig / missing (0.9294ms)
  ✔ loadConfig / null (0.5569ms)
  ✔ loadConfig / array (0.5306ms)
  ✔ loadConfig / string (0.5376ms)
  ✔ loadConfig / number (0.5326ms)
  ✔ saveConfig / missing (0.6447ms)
  ✔ saveConfig / null (0.6267ms)
  ✔ saveConfig / array (0.6117ms)
  ✔ saveConfig / string (0.595ms)
  ✔ saveConfig / number (0.5436ms)
✔ bridge rejects missing or invalid config rather than fabricating success (6.7964ms)
▶ bridge requires an explicit ok:true acknowledgement
  ✔ undefined (0.671ms)
  ✔ false (0.5804ms)
  ✔ 0 (0.5652ms)
  ✔ true (0.5952ms)
✔ bridge requires an explicit ok:true acknowledgement (2.766ms)
✔ bridge ignores wrong tokens, window sources, and request identities (6.7874ms)
✔ requests without a bridge token fail immediately without posting (1.4ms)
✔ bridge timeouts reject instead of leaving a save unresolved (0.7662ms)
ℹ tests 33
ℹ suites 0
ℹ pass 33
ℹ fail 0
ℹ cancelled 0
ℹ skipped 0
ℹ todo 0
ℹ duration_ms 118.1116
==> go run ./tools/packager -targets windows/amd64,linux/amd64 -signing-key build/keys/publisher.private -key-id oai-basispoints-v1
signed package written: dist\local.oai-basispoints-0.5.12.s2plugin
plugin: local.oai-basispoints 0.5.12 (2 runtimes, 6 files)
==> python tools/verify_package.py dist\local.oai-basispoints-0.5.12.s2plugin --public-key build/keys/publisher.public
插件: local.oai-basispoints 0.5.12
包大小: 12674738 字节，条目数: 8
  manifest.json                              1892 字节
  runtimes/linux-amd64/oai-basispoints       15392928 字节
  runtimes/windows-amd64/oai-basispoints.exe 15885312 字节
  signature.json                             170 字节
  ui/assets/app.css                          3817 字节
  ui/assets/app.js                           11946 字节
  ui/assets/bridge-v1.js                     6366 字节
  ui/index.html                              1437 字节
签名: 有效（key_id=oai-basispoints-v1，ed25519）
校验结果: 通过

~~~
