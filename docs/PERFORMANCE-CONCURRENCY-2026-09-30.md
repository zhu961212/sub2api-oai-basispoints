# 2026-09-30 性能与并发优化

本轮基于当前工作区增量修改，保留之前尚未提交的功能修复。修改范围为插件自身的连接复用和请求内协议处理，不改变宿主账号调度、BPS 限流策略或真实模型推理能力。

## 连接复用

internal/transport/transport.go 的每上游空闲连接保留数从 20 调整为 64；每个 Transport 的总空闲连接上限仍为 100，闲置回收仍为 90 秒。直连与按完整代理 URL 隔离的连接池采用相同配置。代理池容量仍最多 128 项、5 分钟闲置淘汰；淘汰只关闭空闲连接。

这不是活动请求并发数上限：没有新增 MaxConnsPerHost 限制，也没有改变账号、代理凭据或请求超时。单上游持续突发时可能保留更多空闲 socket，以减少下一波请求重新建立 TCP/TLS 连接的成本。

### 实际连接回归

新增 TestClientRetainsConnectionsAcrossConcurrentBursts，分别覆盖直连与 HTTP 代理，连续三波、每波 64 个请求。每波使用屏障确认全部请求已到上游后才释放响应，避免把顺序请求误判为并发。

- 原配置在第二波累计建立 108 条连接，即重新建立 44 条连接。
- 新配置三波累计始终为 64 条连接，后两波没有新建连接。
- 缓存、配置更新、Shutdown 及请求隔离等相关回归重复 5 轮通过。

### 本地 TLS 基准

BenchmarkTLSConnectionBursts 在同一进程分别使用旧 20 条与新 64 条保留策略；localhost TLS HTTP/1.1，上游不做模型生成，每个操作是一波 64 请求，预热不计入测量。Windows/amd64、Go 1.27.1，400ms × 3，三组中位数：

| 每波 64 请求 | 原策略 | 新策略 |
| --- | ---: | ---: |
| 耗时 | 19.232 ms | 0.761 ms |
| 新建连接 | 44 | 0 |
| 分配内存 | 约 6.50 MB | 约 455 KB |

以上主要衡量重复 TLS 握手成本，不能换算成 BPS 模型推理速度、真实网络延迟或生产吞吐能力；HTTP/2 的连接复用行为也不同。复现时请避免其他基准同时争用 CPU。

    go test ./internal/transport -run TestClientRetainsConnectionsAcrossConcurrentBursts -count=5 -timeout 120s
    go test ./internal/transport -run '^$' -bench BenchmarkTLSConnectionBursts -benchmem -benchtime=400ms -count=3

## 请求内 JSON 复制

新增 internal/protocol/json_clone.go，仅替换请求内 cloneJSONValue 的常用 JSON 树复制路径。map/list 逐层创建独立容器，不可变字符串复用；避免先序列化为 JSON 再反序列化。长期缓存继续使用原有拥有独立存储的快照逻辑，不改变缓存预算、工具声明、完整历史或可信会话隔离。

类型化容器、自定义序列化、无效 UTF-8、循环和超深结构继续回退原 encoding/json 路径；空容器、typed nil、空 json.Number、超过 2^53 的整数及控制字符保持原归一化语义。请求级复制不套用 4 MiB 缓存准入限制，超过该限制的历史仍完整保留。

固定 64 工具目录，Windows/amd64、i7-9700、Go 1.27.1，前后同参数 500ms × 3，各列独立取中位数：

| 场景 | 原路径 | 优化后 |
| --- | ---: | ---: |
| 请求准备耗时 | 1.120 ms | 0.396 ms（减少约 64.6%） |
| 请求准备分配 | 921,455 B / 4,260 次 | 455,598 B / 1,965 次 |
| 目录复制，串行 | 608.5 μs | 104.7 μs |
| 目录复制，并发 | 215.3 μs/op | 45.9 μs/op |

目录复制对照在同一进程保留原 JSON 往返实现；请求准备使用修改前后相同既有基准。并发 ns/op 是整个进程吞吐的摊销值，不是单请求延迟。缓存命中基准补充预热断言，确保真实目录与原生调用已缓存；缓存读取算法本身未改，不宣称其性能改善。

    go test ./internal/protocol -run '^$' -bench 'Benchmark(ContextPrepareResponses|ProtocolJSONClone|ProtocolCatalogCacheHit|ProtocolNativeCacheHit)$' -benchmem -benchtime=500ms -count=3

## 流式读缓冲复用

直通与转换 SSE 共享 32 KiB scratch 缓冲池，归还前清零。只复用临时读取空间；RPC 帧及保留的 SSE 记录继续独立复制。转换流由外层处理函数拥有缓冲，在关闭响应体、等待读取协程退出且当前数据处理结束后才归还，覆盖最后一块数据与 EOF 同时返回的情况。sync.Pool 可随 GC 回收，不是活跃请求内存的硬上限。

64 个 worker、每个 4 次调用的回归保留全部 256 份发送帧至所有调用结束后核对，覆盖直通与转换 SSE，保证晚到的读取或缓冲复用不会改变之前的响应。

纯内存并发流基准，400ms × 3，中位数；不包含 HTTP/gRPC 网络及模型生成：

| 响应大小 | 原耗时 | 新耗时 | 原分配 | 新分配 |
| --- | ---: | ---: | ---: | ---: |
| 1 KiB | 8.194 μs/op | 0.747 μs/op | 34,734 B/op | 1,980 B/op |
| 32 KiB | 14.003 μs/op | 6.738 μs/op | 66,496 B/op | 33,899 B/op |

    go test ./internal/transport -run '^$' -bench BenchmarkHTTPResponseStreamParallel -benchmem -benchtime=400ms -count=3

完整转换管线每次分配约少 32 KiB；其时间未见稳定改善，且同期协议优化会影响此管线，因此不把综合耗时归因于缓冲池。

## 原始记录

以下为本地忽略目录中的测量材料，不随源码导出：

- dist/protocol-json-clone-before-2026-09-30.txt、protocol-json-clone-final-2026-09-30.txt：请求准备前后及同进程复制对照。
- dist/protocol-json-clone-test-2026-09-30.txt、protocol-json-clone-fuzz-2026-09-30.txt：协议回归与有限 fuzz 记录。
- dist/transport-performance-baseline-2026-09-30.txt：TLS 对照及旧配置失败复现。
- dist/transport-stream-before-2026-09-30.txt、transport-stream-after-2026-09-30.txt：读缓冲优化前后。
- dist/transport-performance-commands-2026-09-30.txt：传输层完整测量命令及边界。

## BPS 模型核查

2026-09-30 北京时间 21:31 至 21:32，使用现有账号只读查询两条官方目录接口，三次请求均返回 HTTP 200 且模型列表一致。与 2026-09-26 的六模型基线相比，新增 0、删除 0；没有据此修改插件可选模型。详见 [BPS 实时模型核查](BPS-MODELS-2026-09-30.md)。

目录可见性属于本次账号与请求时间，不证明每个账号拥有相同权限，也不等于逐模型生成测试；本次核查没有发送生成请求。

## 最终验证与构建

- 全量 Go 回归通过：go test ./... -skip TestLive -count=1 -timeout 120s。
- go vet ./...、UI 两个 JS 文件语法检查、git diff --check 通过。
- 既有 UI 行为测试 305 项、Python 工具及验包测试 41 项通过。
- 协议等价性有限 fuzz 执行 133,167 次，无失败；32 调用复制隔离、超缓存预算完整历史和 JSON 归一化回归通过。
- 连接池及流式隔离相关测试分别重复 5 轮通过。
- 修正了一项既有附件测试的时序波动：其 10ms 实际超时会在 CPU 忙碌时早于 PNG 准备完成。改用仓库已有 testing/synctest 虚拟时钟，在响应读取等待时触发真实 context 超时，所有原有取消、关闭响应体、一次请求及诊断断言保留。该测试重复 50 次及随后全量回归均通过，附件运行时代码未因这个测试调整。

本机完整 Forward 测试分别覆盖转换与直通两种模式、1/8/32/64 并发，共 2,048 个计时样本，全部零错误；每档实际峰值达到目标并发。上游固定首包前等待 10ms、完成前再等待 10ms，因此以下结果只说明本机模拟场景可并发完成，不是生产容量或真实模型延迟：

| 64 并发模式 | 样本 | 总耗时 p95 | 首包 p95 | 错误 |
| --- | ---: | ---: | ---: | ---: |
| 转换 SSE | 512 | 24.387 ms | 12.850 ms | 0 |
| 直通 | 512 | 23.078 ms | 12.854 ms | 0 |

原始记录：dist/forward-performance-final-2026-09-30.txt。此项只跑了修改后的集成检查，不作为前后速度比较。

Windows/amd64 和 Linux/amd64 均已交叉编译，独立 Python 校验器验证了清单及全部文件哈希。测试包位于 dist/performance-validation-20260930/local.oai-basispoints-0.6.10-performance.s2plugin，包含 2 个运行时、6 个清单文件。版本保持 0.6.10；这是未签名本地调试产物，正式宿主默认不会接受。本轮没有发布或部署。

## 竞态检测环境

本机 CGO_ENABLED=0，未发现可用的 GCC/Clang/Zig，WSL Linux 环境也未就绪。因此本轮本机不能执行 Go race 检测；项目既有 CI 的 Linux go test -race 作业保留，未将其未执行结果描述为通过。
