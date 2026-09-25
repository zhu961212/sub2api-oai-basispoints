package transport

// 真实环境实测（默认跳过）。只有显式提供凭据时才跑，用来复验插件对真实
// Basis Points 的行为，例如推理强度映射与模型路由的必要性。
//
//	BASISPOINTS_LIVE_TOKEN       access token
//	BASISPOINTS_LIVE_ACCOUNT_ID  chatgpt account id
//	BASISPOINTS_LIVE_PROXY       可选的 socks5 / http 代理 URL
//
//	go test ./internal/transport/ -run TestLive -v -count=1 -timeout 240s
//
// 注意：会真实消耗账号额度，请按需运行。

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"golang.org/x/net/proxy"
)

func liveCredentials(t *testing.T) (string, string, string) {
	t.Helper()
	token := os.Getenv("BASISPOINTS_LIVE_TOKEN")
	accountID := os.Getenv("BASISPOINTS_LIVE_ACCOUNT_ID")
	if token == "" || accountID == "" {
		t.Skip("需要 BASISPOINTS_LIVE_TOKEN 与 BASISPOINTS_LIVE_ACCOUNT_ID")
	}
	return token, accountID, os.Getenv("BASISPOINTS_LIVE_PROXY")
}

func liveClient(t *testing.T, proxyURL string) *http.Client {
	t.Helper()
	transport := &http.Transport{ForceAttemptHTTP2: true}
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Host == "" {
			t.Fatalf("invalid proxy url: %v", err)
		}
		if strings.HasPrefix(parsed.Scheme, "socks5") {
			var auth *proxy.Auth
			if parsed.User != nil {
				auth = &proxy.Auth{User: parsed.User.Username()}
				if password, ok := parsed.User.Password(); ok {
					auth.Password = password
				}
			}
			dialer, err := proxy.SOCKS5("tcp", parsed.Host, auth, proxy.Direct)
			if err != nil {
				t.Fatalf("socks5 dialer: %v", err)
			}
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
					return contextDialer.DialContext(ctx, network, address)
				}
				return dialer.Dial(network, address)
			}
		} else {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	return &http.Client{Transport: transport, Timeout: 150 * time.Second}
}

// livePost 走插件自身的请求构造与头，把 source 原样送到真实端点。
func livePost(t *testing.T, source map[string]any) (int, []byte, map[string]any) {
	t.Helper()
	token, accountID, proxyURL := liveCredentials(t)
	cfg := protocol.DefaultConfig()
	upstreamBody, err := protocol.PrepareResponsesBody(source, cfg)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Logf("插件发给上游的 reasoning_effort=%v model=%v", upstreamBody["reasoning_effort"], upstreamBody["model"])

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cfg.ResponsesURL, strings.NewReader(string(protocol.JSONBytes(upstreamBody))))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("ChatGPT-Account-ID", accountID)
	request.Header.Set("X-OpenAI-Account-ID", accountID)
	request.Header.Set("X-Basispoints-Auth-Mode", cfg.AuthMode)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	for key, value := range basisPointsHeaders() {
		request.Header.Set(key, value)
	}

	started := time.Now()
	response, err := liveClient(t, proxyURL).Do(request)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	t.Logf("HTTP %d 耗时 %s %d 字节", response.StatusCode, time.Since(started).Round(time.Millisecond), len(payload))
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, payload, nil
	}
	final, err := protocol.ParseFinalStreamResponse(payload)
	if err != nil {
		t.Fatalf("parse stream: %v", err)
	}
	return response.StatusCode, payload, final
}

func liveMessageSource(model, effortField, effort string) map[string]any {
	source := map[string]any{
		"model":  model,
		"stream": true,
		"store":  false,
		"input":  []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Say OK"}}}},
	}
	if effortField == "reasoning" {
		source["reasoning"] = map[string]any{"effort": effort}
	} else if effort != "" {
		source[effortField] = effort
	}
	return source
}

// 客户端写 ultra / max / xhigh 时，上游必须收到 xhigh（并确认按 xhigh 执行）。
func TestLiveEffortMappingReachesXHigh(t *testing.T) {
	for _, tc := range []struct{ field, value string }{
		{"reasoning", "ultra"},
		{"reasoning", "max"},
		{"reasoning_effort", "ultra"},
		{"reasoning_effort", "xhigh"},
	} {
		t.Run(tc.field+"="+tc.value, func(t *testing.T) {
			source := liveMessageSource("gpt-6-astra", tc.field, tc.value)
			status, payload, final := livePost(t, source)
			if status != http.StatusOK {
				t.Fatalf("上游拒绝：%s", previewText(string(payload), 300))
			}
			reasoning, _ := final["reasoning"].(map[string]any)
			effort := protocol.StringValue(reasoning["effort"])
			if effort != "xhigh" {
				t.Fatalf("上游确认的 effort = %q，want xhigh；reasoning=%s",
					effort, previewText(string(protocol.JSONBytes(final["reasoning"])), 200))
			}
			t.Logf("上游确认 effort=%s", effort)
		})
	}
}

// 真实请求只使用 gpt-6-astra；其他模型的透传在本地回归测试覆盖。
func TestLiveAstraModelIsAcceptedUpstream(t *testing.T) {
	source := liveMessageSource("gpt-6-astra", "reasoning", "low")
	status, payload, _ := livePost(t, source)
	if status != http.StatusOK {
		t.Fatalf("上游拒绝 gpt-6-astra：HTTP %d %s", status, previewText(string(payload), 240))
	}
}

// 用真实上游响应跑一遍插件自己的流式转换，验证交给客户端的流是完整的：
// 有 response.created、有 response.completed、有 data: [DONE]，并以 End 帧收尾，
// 且不出现 error 帧 —— 即客户端不会在中途看到断流。
func TestLiveTransformedStreamIsComplete(t *testing.T) {
	token, accountID, proxyURL := liveCredentials(t)
	cfg := protocol.DefaultConfig()
	source, err := protocol.RawObject([]byte(`{
	  "model": "gpt-6-astra",
	  "stream": true,
	  "store": false,
	  "reasoning": {"effort": "low"},
	  "input": [{"role": "user", "content": [{"type": "input_text", "text": "Call the get_weather tool with city=Beijing through the run_officejs transport, then answer with the result."}]}],
	  "tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody, err := protocol.PrepareResponsesBody(source, cfg)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cfg.ResponsesURL, strings.NewReader(string(protocol.JSONBytes(upstreamBody))))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("ChatGPT-Account-ID", accountID)
	request.Header.Set("X-OpenAI-Account-ID", accountID)
	request.Header.Set("X-Basispoints-Auth-Mode", cfg.AuthMode)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	for key, value := range basisPointsHeaders() {
		request.Header.Set(key, value)
	}
	response, err := liveClient(t, proxyURL).Do(request)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("上游返回 %d：%s", response.StatusCode, previewText(string(payload), 300))
	}

	// 交给插件真实的流式转换路径（与生产同一条代码路径）。
	stub := &streamStub{ctx: context.Background()}
	if err := sendTransformedHTTPResponseStream(stub, response, cfg.MaxResponseBytes, source); err != nil {
		t.Fatalf("插件流式转换失败：%v", err)
	}

	var stream strings.Builder
	ended := false
	errFrame := ""
	for _, frame := range stub.responses {
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			stream.Write(chunk)
		}
		if frame.GetEnd() != nil {
			ended = true
		}
		if failure := frame.GetError(); failure != nil {
			errFrame = failure.GetCode() + ": " + failure.GetMessage()
		}
	}
	if errFrame != "" {
		t.Fatalf("插件输出了错误帧（客户端会看到断流）：%s", errFrame)
	}
	if !ended {
		t.Fatal("插件没有发送结束帧")
	}
	out := stream.String()
	for _, want := range []string{"response.created", "response.completed", "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出流缺少 %s（%d 字节）", want, len(out))
		}
	}
	// 上游会在响应里带 run_officejs 的工具声明（那是 Basis Points 的原生工具），
	// 不算泄漏；真正的泄漏是 output_item 里出现原生传输调用。
	decoder := &sseRelayDecoder{}
	if err := decoder.feed([]byte(out), func(event sseRelayEvent) error {
		if event.data == "" || event.data == "[DONE]" {
			return nil
		}
		payload, err := protocol.RawObject([]byte(event.data))
		if err != nil {
			return nil
		}
		item := relayObject(payload["item"])
		if protocol.StringValue(item["type"]) == "function_call" && isNativeRelayName(protocol.StringValue(item["name"])) {
			t.Fatalf("output_item 里泄漏了原生传输调用：%s", previewText(event.data, 240))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("插件输出 %d 字节：含 created / completed / [DONE]，以 End 帧收尾，无错误帧，无原生调用泄漏", len(out))
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func previewText(value string, limit int) string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\r", "")
	if len(value) > limit {
		return value[:limit] + "…"
	}
	return value
}
