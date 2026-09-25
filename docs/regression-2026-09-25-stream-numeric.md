# 2026-09-25 流式拷贝与数值校验修复复测

测试环境：Windows amd64，Go 1.27.1，Intel Core i7-9700，8 CPUs。
所有请求均使用 localhost 模拟上游，不使用真实凭据。

## 修复

- 传输层与协议层 SSE 解码器只扫描新到达的分块；半行数据累积到行结束，避免每次读取重新复制整行。
- 保留 CRLF、UTF-8 分片、多行 data、原始事件字节、输入缓冲复用与 EOF 补齐行为。
- 工具参数的 integer 按数学整数判断；1.0、1e0 通过，1.5 拒绝。
- 精确执行 minimum、maximum、exclusiveMinimum、exclusiveMaximum、multipleOf，保留大整数与 enum/const 的既有精度。比较和倍数判断不展开巨大指数。
- 新增流式集成回归：并行工具调用中任一参数不合法时，整批不产生可执行工具事件，返回 invalid_tool_call，并正常结束 SSE。

## 解码器微基准

同一进程配置下每项 3 次迭代，每次输入一个 data 事件，以 32 KiB 分块喂给传输层解码器；计时不含测试数据构造。
B/op 是每次操作累计分配字节数，不是峰值常驻内存。短样本耗时仅作本地诊断。

| 事件大小 | 修复前 ns/op | 修复后 ns/op | 修复前 B/op | 修复后 B/op | 修复前/后 allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1 MiB | 10,777,767 | 1,495,433 | 25,691,965 | 7,331,861 | 48 / 11 |
| 8 MiB | 254,332,200 | 11,613,000 | 1,145,046,709 | 58,712,117 | 287 / 14 |

1 MiB 行按 4 KiB 分块的分配回归在旧实现上复现 272 次分配（超过 64 的宽松上限），修复后通过。8 MiB 微基准累计分配减少约 94.9%。

复现命令：

~~~powershell
go test ./internal/transport -run '^$' -bench '^BenchmarkSSERelayDecoderFragmented$' -benchtime=3x -benchmem -count=1
go test ./internal/protocol -run '^$' -bench '^BenchmarkSSEDecoderFragmentedLine$' -benchmem
~~~

## 最终验证

- 全部本地 Go 测试通过：go test ./... -skip TestLive -count=1 -timeout 120s。
- 静态检查 go vet ./... 通过，所有变更的 Go 文件通过 gofmt 格式核对。
- 数值回归包括 38 个请求准备至响应转换用例，以及 3,025 组与 big.Rat 的独立比较和倍数交叉验证。
- 流式数值集成测试覆盖整数表示、上下界拒绝，以及同批合法调用不会在另一调用无效时提前发出。
- 全量测试发现并更新了 6 项旧模型路由断言，按 README 既定的 gpt-6-astra 固定路由规则验证；只修改配置测试，没有改变生产路由。
- 当前 CGO_ENABLED=0，未检测到 gcc，未运行 Go race detector；64 路隔离回归不等同于竞态检测。
- 本次未运行 TestLive，也未发布或部署插件。

~~~text
?   	github.com/wangyunjeff/sub2api-oai-basispoints/cmd/oai-basispoints	[no test files]
ok  	github.com/wangyunjeff/sub2api-oai-basispoints/internal/auth	0.361s
ok  	github.com/wangyunjeff/sub2api-oai-basispoints/internal/config	0.367s
ok  	github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol	0.918s
ok  	github.com/wangyunjeff/sub2api-oai-basispoints/internal/transport	1.463s
?   	github.com/wangyunjeff/sub2api-oai-basispoints/tools/keygen	[no test files]
?   	github.com/wangyunjeff/sub2api-oai-basispoints/tools/packager	[no test files]
~~~

## 64 路隔离回归

直连与 HTTP 代理各运行 64 个并发请求，重复 20 轮，共 2,560 个请求通过。
覆盖成功、提前 EOF、取消、非法工具调用；所有请求共用 call_id，同时使用不同会话和工具目录，检查请求隔离与结束状态。

~~~powershell
go test ./internal/transport -run '^TestForwardConcurrentRequestsRemainIsolated$' -count=20 -timeout 120s
~~~

## 本地延迟复测

模拟上游固定在 10 ms 后开始输出，再延迟 10 ms 完成；转换与透传两种模式，在 1、8、32、64 并发下均通过，errors=0。
延迟包含 Forward、localhost HTTP 和模拟等待；不包含请求数据构造、独立插件进程和 gRPC 边界。吞吐为分批运行测量，并非饱和负载测试。
这些数值不能表示真实模型推理速度、广域网延迟或生产并发能力。

~~~powershell
$env:BASISPOINTS_LOCAL_PERF='1'
go test ./internal/transport -run '^TestLocalForwardPerformance$' -count=1 -v -timeout 120s
~~~

## 解码微基准原始输出

~~~text
goos: windows
goarch: amd64
pkg: github.com/wangyunjeff/sub2api-oai-basispoints/internal/transport
cpu: Intel(R) Core(TM) i7-9700 CPU @ 3.00GHz
BenchmarkSSERelayDecoderFragmented/1MiB-8         	       3	   1495433 ns/op	 701.19 MB/s	 7331861 B/op	      11 allocs/op
BenchmarkSSERelayDecoderFragmented/8MiB-8         	       3	  11613000 ns/op	 722.35 MB/s	58712117 B/op	      14 allocs/op
PASS
ok  	github.com/wangyunjeff/sub2api-oai-basispoints/internal/transport	0.331s

~~~

## 本地延迟原始输出

~~~text
=== RUN   TestLocalForwardPerformance
    performance_test.go:43: LOCAL ONLY runtime=go1.27.1 GOOS=windows GOARCH=amd64 CPUs=8 GOMAXPROCS=8 first_delay=10ms completion_delay=10ms
=== RUN   TestLocalForwardPerformance/transformed/concurrency=1
    performance_test.go:150: LOCAL mode=transformed concurrency=1 samples=128 peak_upstreams=1 total_p50=21.621ms total_p95=22.377ms first_p50=10.819ms first_p95=11.505ms throughput=46.1_req/s errors=0
=== RUN   TestLocalForwardPerformance/transformed/concurrency=8
    performance_test.go:150: LOCAL mode=transformed concurrency=8 samples=128 peak_upstreams=8 total_p50=21.927ms total_p95=22.534ms first_p50=11.291ms first_p95=12.129ms throughput=356.1_req/s errors=0
=== RUN   TestLocalForwardPerformance/transformed/concurrency=32
    performance_test.go:150: LOCAL mode=transformed concurrency=32 samples=256 peak_upstreams=32 total_p50=22.533ms total_p95=24.415ms first_p50=11.81ms first_p95=13.793ms throughput=1309.4_req/s errors=0
=== RUN   TestLocalForwardPerformance/transformed/concurrency=64
    performance_test.go:150: LOCAL mode=transformed concurrency=64 samples=512 peak_upstreams=64 total_p50=23.532ms total_p95=25.752ms first_p50=12.783ms first_p95=14.686ms throughput=2353.5_req/s errors=0
=== RUN   TestLocalForwardPerformance/passthrough/concurrency=1
    performance_test.go:150: LOCAL mode=passthrough concurrency=1 samples=128 peak_upstreams=1 total_p50=21.208ms total_p95=22.516ms first_p50=10.691ms first_p95=11.647ms throughput=46.9_req/s errors=0
=== RUN   TestLocalForwardPerformance/passthrough/concurrency=8
    performance_test.go:150: LOCAL mode=passthrough concurrency=8 samples=128 peak_upstreams=8 total_p50=21.736ms total_p95=23.099ms first_p50=11.098ms first_p95=13.035ms throughput=356.7_req/s errors=0
=== RUN   TestLocalForwardPerformance/passthrough/concurrency=32
    performance_test.go:150: LOCAL mode=passthrough concurrency=32 samples=256 peak_upstreams=32 total_p50=21.186ms total_p95=22.14ms first_p50=10.81ms first_p95=11.629ms throughput=1425.9_req/s errors=0
=== RUN   TestLocalForwardPerformance/passthrough/concurrency=64
    performance_test.go:150: LOCAL mode=passthrough concurrency=64 samples=512 peak_upstreams=64 total_p50=22.845ms total_p95=28.734ms first_p50=12.31ms first_p95=18.579ms throughput=2363.4_req/s errors=0
--- PASS: TestLocalForwardPerformance (7.24s)
    --- PASS: TestLocalForwardPerformance/transformed/concurrency=1 (2.80s)
    --- PASS: TestLocalForwardPerformance/transformed/concurrency=8 (0.38s)
    --- PASS: TestLocalForwardPerformance/transformed/concurrency=32 (0.22s)
    --- PASS: TestLocalForwardPerformance/transformed/concurrency=64 (0.25s)
    --- PASS: TestLocalForwardPerformance/passthrough/concurrency=1 (2.75s)
    --- PASS: TestLocalForwardPerformance/passthrough/concurrency=8 (0.38s)
    --- PASS: TestLocalForwardPerformance/passthrough/concurrency=32 (0.21s)
    --- PASS: TestLocalForwardPerformance/passthrough/concurrency=64 (0.24s)
PASS
ok  	github.com/wangyunjeff/sub2api-oai-basispoints/internal/transport	7.497s

~~~
