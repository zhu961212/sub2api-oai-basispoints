# 0.5.15 图片中继性能记录

测量日期：2026-09-25。以下结果用于比较此次修改前后的本地图片中继处理开销。基准直接调用 Relay.Rewrite 和 Relay.ServeHTTP，不包含 JSON 解析、完整请求转发、网络上传、WAN、TLS 握手或真实 Basispoints / BPS 上游推理耗时。样本数量较小，不能作为生产 SLA 或端到端响应速度承诺。

## 测量环境

- 系统：Windows 10，版本 10.0.19045.0，windows/amd64。
- CPU：Intel Core i7-9700 @ 3.00 GHz。
- Go：go1.27.1 windows/amd64。基准名后缀 -8 对应 GOMAXPROCS=8；本组基准逐次运行，没有启用 RunParallel。
- 包：github.com/wangyunjeff/sub2api-oai-basispoints/internal/imagerelay。
- 文件存储：本机临时目录；结果受本机磁盘、系统文件缓存和其他进程负载影响，没有清空系统文件缓存。

## 输入夹具与计时边界

所有场景使用同一生成规则的有效 PNG，不包含真实用户图片、账户信息或凭据。源码见 internal/imagerelay/performance_test.go。

- 尺寸：1024 × 1024。底层图像为 NRGBA，各像素 alpha 固定为 255。
- RGB 值：32 位 xorshift 状态机，固定初始种子 0x9e3779b9；依次执行左移 13、右移 17、左移 5 的异或变换，为每个 RGB 通道取低 8 位。
- 编码器：Go image/png，CompressionLevel = png.NoCompression。
- 本环境编码后的精确 PNG 大小：3,148,214 字节，约 3 MiB。
- Base64 长度：4,197,620 字节；加 data:image/png;base64, 前缀后为 4,197,642 字节。
- PNG SHA-256：9e3aaf77183d225135af64b27ad5bf1d1c38d4a6d411d3180164d55253e41f3e。

图片生成、PNG 编码、Base64 编码、初始请求 map、Relay 创建及同 scope / 下载预热均在 ResetTimer 前完成。循环内恢复 image_url；首次场景还生成唯一 scope，二者微小开销包含在计时中。所有基准均调用 ReportAllocs。

首次场景和同请求重复场景每次使用新的 scope，确保经过新图片存储路径。每累计 32 次操作，在 StopTimer / StartTimer 之间关闭旧 Relay 并创建新会话，防止长时间基准达到存储配额；没有每次操作新建磁盘目录。一个批次最多保留 32 份独立图片，约 96 MiB。

四种场景定义：

| 基准 | 含义 |
| --- | --- |
| BenchmarkRewriteFirst3MiB | 每次以新 scope 首次 Rewrite 一张 PNG，包含中继校验和落盘。 |
| BenchmarkRewriteSameScope3MiB | 先成功 Rewrite 预热，再以同 scope 重发相同的完整 data URL。 |
| BenchmarkRewriteDuplicate4x3MiB | 每次新 scope，同一个请求放入四个相同的完整 data URL。 |
| BenchmarkDownload3MiB | 预先存入图片，直接调用 ServeHTTP；自定义 ResponseWriter 只统计收到的字节，不保留响应体。 |

下载场景没有建立网络连接，因此表中下载结果是本地文件读取与 HTTP handler 的开销。同请求重复场景的 SetBytes 按四份逻辑输入计数；其 MB/s 不代表四份物理落盘流量。

## 前后对比

每项使用 -benchtime=5x -count=3；一组执行 5 次，表内为三组输出各字段独立取中位数。ms/op 是平均每次操作耗时的组间中位数；B/op 与 allocs/op 是 Go 基准报告的堆分配，不是常驻内存、峰值内存或磁盘占用。

| 场景 | 修改前 ms/op | 修改后 ms/op | 耗时下降 | 修改前 B/op | 修改后 B/op | 修改前 allocs/op | 修改后 allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 首次 Rewrite，一张约 3 MiB | 32.61644 | 17.98940 | 44.8% | 110,523 | 82,212 | 41 | 36 |
| 同 scope 重发 | 14.05986 | 10.23710 | 27.2% | 36,033 | 777 | 20 | 13 |
| 同请求四张重复图片 | 73.61680 | 17.80844 | 75.8% | 218,971 | 82,795 | 100 | 47 |
| 本地下载 handler | 1.65686 | 1.45772 | 12.0% | 66,129 | 33,352 | 11 | 10 |

耗时下降按 1 - 修改后中位数 / 修改前中位数计算。该组数据中，首次 Rewrite 约为修改前的 1.81 倍处理速度，四重复图片约为 4.13 倍；这些比例仅适用于本基准条件。

原始控制台记录保存在本次工作目录下的 dist/image-performance-before.txt 和 dist/image-performance-after.txt。dist 内的基线源码和测量日志是本地复现材料，不是运行时依赖。

## 源码基线隔离与复现

本次基线来自优化开始前保存的 relay.go、storage.go 和 http.go，原样放在 dist/image-baseline-source/。基准运行时没有恢复或覆盖共享生产源码。

隔离模块位于 dist/image-baseline-work/：

1. 保持原模块路径 github.com/wangyunjeff/sub2api-oai-basispoints。
2. internal/imagerelay 下放入上述三个旧文件，以及相同的 lease_windows.go、lease_unix.go 和本次 performance_test.go。
3. 使用相同 Go 工具链，go.mod 指定 go 1.27、golang.org/x/image v0.41.0 与 golang.org/x/sys v0.47.0；复用仓库 go.sum。
4. 先运行隔离旧模块，再在工作仓库运行优化后模块，命令参数完全一致。

三个旧文件的 SHA-256：

| 文件 | SHA-256 |
| --- | --- |
| relay.go | 3f6656499ea4771d3b13891cb0ab91e778360eb6c5d3de6a08cd7e6a5846f5e9 |
| storage.go | 0ecdd17174384dce7eb813e5522dd045ee6ddeefd1a7d324bcbc7e11d8850de27 |
| http.go | 9e4d15b768d00523cab290e96931d5fb7ac8983d8996264d6a792330e845f09f |

在仓库根目录使用 PowerShell，修改前：

    go -C dist/image-baseline-work test ./internal/imagerelay -run '^$' -bench 'Benchmark(Rewrite|Download)' -benchtime=5x -count=3 -timeout=90s 2>&1 | Tee-Object -FilePath dist/image-performance-before.txt

修改后：

    go test ./internal/imagerelay -run '^$' -bench 'Benchmark(Rewrite|Download)' -benchtime=5x -count=3 -timeout=90s 2>&1 | Tee-Object -FilePath dist/image-performance-after.txt

两次运行均 PASS。使用更长迭代次数或更多重复组可减少短样本波动，但应保持前后相同参数、硬件、工具链及输入生成规则。

## 跨 32 次批次的检查

为验证长期冷路径不会因图片积累而达到存储配额，额外运行 33 次首次 Rewrite，强制在第 33 次操作前经过一次关闭、清理与新建会话：

    go test ./internal/imagerelay -run '^$' -bench '^BenchmarkRewriteFirst3MiB$' -benchtime=33x -count=1 -timeout=60s

该检查 PASS，输出为 18,139,385 ns/op、76,398 B/op、35 allocs/op。它验证批次轮换可工作，不并入前三组的前后中位数统计。

## 适用范围

本记录覆盖单机、顺序执行、固定 PNG 的 Relay 热路径与冷路径。它不证明不同图片格式、不同大小、并发负载、网络环境或上游模型服务下具有相同收益，也不衡量 JSON 解析、WAN 传输、真实 BPS 推理或完整用户可感知延迟。5 次迭代 × 3 组属于短样本，不能据此建立生产 SLA、尾延迟或稳定吞吐承诺。
