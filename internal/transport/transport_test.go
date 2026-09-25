package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/config"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// streamStub 是 TransportPlugin_ForwardServer 的测试替身：Recv 回放预置请求帧，
// Send 收集响应帧。
type streamStub struct {
	ctx       context.Context
	requests  []*pluginv1.ForwardRequest
	index     int
	responses []*pluginv1.ForwardResponse
}

type timingStreamStub struct {
	streamStub
	firstBodyChunk chan time.Time
}

func (s *timingStreamStub) Send(response *pluginv1.ForwardResponse) error {
	if chunk := response.GetBodyChunk(); len(chunk) > 0 && s.firstBodyChunk != nil {
		select {
		case s.firstBodyChunk <- time.Now():
		default:
		}
	}
	return s.streamStub.Send(response)
}

func (s *streamStub) Send(response *pluginv1.ForwardResponse) error {
	s.responses = append(s.responses, response)
	return nil
}

func (s *streamStub) Recv() (*pluginv1.ForwardRequest, error) {
	if s.index >= len(s.requests) {
		return nil, io.EOF
	}
	request := s.requests[s.index]
	s.index++
	return request, nil
}

func (s *streamStub) Context() context.Context     { return s.ctx }
func (s *streamStub) SetHeader(metadata.MD) error  { return nil }
func (s *streamStub) SendHeader(metadata.MD) error { return nil }
func (s *streamStub) SetTrailer(metadata.MD)       {}
func (s *streamStub) SendMsg(any) error            { return nil }
func (s *streamStub) RecvMsg(any) error            { return nil }

type forwardResult struct {
	status   int
	headers  map[string]*pluginv1.HeaderValues
	body     []byte
	errFrame *pluginv1.ForwardResponseError
	ended    bool
	received int64
}

func runForward(t *testing.T, transport *Transport, requests []*pluginv1.ForwardRequest) forwardResult {
	t.Helper()
	stub := &streamStub{ctx: context.Background(), requests: requests}
	if err := transport.Forward(stub); err != nil {
		t.Fatalf("Forward returned transport error: %v", err)
	}
	result := forwardResult{}
	if len(stub.responses) == 0 {
		t.Fatal("no response frames were sent")
	}
	for _, frame := range stub.responses {
		if start := frame.GetStart(); start != nil {
			result.status = int(start.GetStatusCode())
			result.headers = start.GetHeaders()
		}
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			result.body = append(result.body, chunk...)
		}
		if end := frame.GetEnd(); end != nil {
			result.ended = true
			result.received = end.GetBytesReceived()
		}
		if failure := frame.GetError(); failure != nil {
			result.errFrame = failure
		}
	}
	return result
}

func token(t *testing.T, accountID string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"exp":                         4102444800,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	})
	if err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]any{"alg": "none"})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
}

func requestFrames(t *testing.T, url, accessToken string, headers map[string]string, body []byte) []*pluginv1.ForwardRequest {
	t.Helper()
	values := map[string]*pluginv1.HeaderValues{}
	for key, value := range headers {
		values[key] = &pluginv1.HeaderValues{Values: []string{value}}
	}
	frames := []*pluginv1.ForwardRequest{{
		Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{
			RequestId:   "req-test",
			Method:      http.MethodPost,
			Url:         url,
			Headers:     values,
			AccountId:   7,
			Platform:    "openai",
			AccountType: "oauth",
		}},
	}}
	if accessToken != "" {
		if frames[0].GetStart().Headers == nil {
			frames[0].GetStart().Headers = map[string]*pluginv1.HeaderValues{}
		}
		frames[0].GetStart().Headers["Authorization"] = &pluginv1.HeaderValues{Values: []string{"Bearer " + accessToken}}
	}
	if len(body) > 0 {
		frames = append(frames, &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: body}})
	}
	return append(frames, &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}})
}

func applyConfig(t *testing.T, transport *Transport, body map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: raw})
	if err != nil {
		t.Fatal(err)
	}
	if !response.GetApplied() {
		t.Fatalf("ApplyConfig rejected %s: %s", string(raw), response.GetMessage())
	}
}

func TestGetInfoMatchesDeclaredIdentity(t *testing.T) {
	info, err := New().GetInfo(context.Background(), &pluginv1.GetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.GetPluginId() != protocol.PluginID || info.GetPluginVersion() != protocol.Version {
		t.Fatalf("identity = %s %s", info.GetPluginId(), info.GetPluginVersion())
	}
	if info.GetProtocolVersion() != pluginv1.ProtocolVersion || info.GetTransportApiVersion() != pluginv1.TransportAPIVersion {
		t.Fatalf("protocol = %d/%d", info.GetProtocolVersion(), info.GetTransportApiVersion())
	}
	capabilities := info.GetCapabilities()
	if len(capabilities) != 1 || capabilities[0] != protocol.Capability {
		t.Fatalf("capabilities = %#v", capabilities)
	}
}

func TestHealthIsReadOnlySnapshot(t *testing.T) {
	health, err := New().Health(context.Background(), &pluginv1.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !health.GetHealthy() {
		t.Fatalf("plugin reported unhealthy: %s", health.GetMessage())
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(health.GetStatusJson()), &status); err != nil {
		t.Fatalf("status_json is not JSON: %v", err)
	}
	if status["plugin_version"] != protocol.Version || status["capability"] != protocol.Capability {
		t.Fatalf("status = %#v", status)
	}
}

func TestValidateConfigAcceptsEmptyObjectAndRejectsUnknownFields(t *testing.T) {
	transport := New()
	valid, err := transport.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !valid.GetValid() {
		t.Fatalf("empty config rejected: %s", valid.GetMessage())
	}
	var normalized map[string]any
	if err := json.Unmarshal(valid.GetNormalizedConfigJson(), &normalized); err != nil {
		t.Fatalf("normalized config is not JSON: %v", err)
	}
	if normalized["responses_url"] != config.DefaultResponsesURL {
		t.Fatalf("normalized config = %#v", normalized)
	}
	invalid, err := transport.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{
		ConfigJson: []byte(`{"responses_urll":"https://example.test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if invalid.GetValid() {
		t.Fatal("unknown field was accepted")
	}
}

func TestApplyConfigKeepsPreviousConfigOnFailure(t *testing.T) {
	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": "https://first.example.test/basispoints/api/responses"})
	if _, err := transport.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{
		ConfigJson: []byte(`{"responses_url":"ftp://broken.example.test"}`),
	}); err != nil {
		t.Fatal(err)
	}
	health, err := transport.Health(context.Background(), &pluginv1.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(health.GetStatusJson(), "first.example.test") {
		t.Fatalf("previous config was not preserved: %s", health.GetStatusJson())
	}
}

func TestForwardRewritesToolCallsAndSetsBasisPointsHeaders(t *testing.T) {
	captured := make(chan *http.Request, 1)
	capturedBody := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		captured <- request.Clone(context.Background())
		capturedBody <- parsed
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"run_officejs","arguments":"{\"code\":\"{\\\"tool\\\":\\\"demo\\\",\\\"args\\\":{\\\"value\\\":1}}\"}"}]}`))
	}))
	defer upstream.Close()

	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})

	body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"demo","parameters":{"type":"object","properties":{"value":{"type":"integer"}}}}],"reasoning":{"effort":"max"}}`)
	result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct-jwt"), map[string]string{
		"Chatgpt-Account-Id": "acct-header",
		"Accept":             "application/json",
	}, body))
	if result.errFrame != nil {
		t.Fatalf("unexpected error frame: %s", result.errFrame.GetMessage())
	}
	if result.status != http.StatusOK {
		t.Fatalf("status = %d", result.status)
	}
	seen := <-captured
	seenBody := <-capturedBody

	// 上游收到的应是 Basis Points 端点与 Excel 客户端画像头。
	// 服务端 request.URL 只有路径，主机名在 request.Host 上。
	if seen.Host == "" {
		t.Fatal("upstream request did not reach the configured endpoint")
	}
	if seen.Header.Get("Authorization") == "" || seen.Header.Get("ChatGPT-Account-ID") != "acct-header" {
		t.Fatalf("auth headers = %#v", seen.Header)
	}
	if seen.Header.Get("X-OpenAI-Account-ID") != "acct-header" || seen.Header.Get("X-Basispoints-Auth-Mode") != "chatgpt" {
		t.Fatalf("basis points headers = %#v", seen.Header)
	}
	if seen.Header.Get("Origin") != "https://bps.openai.com" || seen.Header.Get("X-OpenAI-Internal-Basispoints-Client-Product") != "basispoints-excel-plugin" {
		t.Fatalf("client profile headers = %#v", seen.Header)
	}
	if _, exists := seenBody["tools"]; exists {
		t.Fatalf("upstream body still carries tools: %#v", seenBody)
	}
	if seenBody["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh", seenBody["reasoning_effort"])
	}

	// 返回给客户端的是真实工具调用，而不是 run_officejs 传输调用。
	var clientResponse map[string]any
	if err := json.Unmarshal(result.body, &clientResponse); err != nil {
		t.Fatalf("response body is not JSON: %v (%s)", err, result.body)
	}
	output := clientResponse["output"].([]any)
	call := output[0].(map[string]any)
	if call["name"] != "demo" || call["call_id"] != "call_1" {
		t.Fatalf("client tool call = %#v", call)
	}
	if !result.ended || result.received != int64(len(result.body)) {
		t.Fatalf("end frame = %+v, body length = %d", result.ended, len(result.body))
	}
}

func TestForwardFallsBackToTokenAccountID(t *testing.T) {
	accountIDs := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		accountIDs <- request.Header.Get("ChatGPT-Account-ID")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct-from-jwt"), nil, []byte(`{"model":"gpt-6-astra","input":[]}`)))
	if result.errFrame != nil {
		t.Fatalf("unexpected error frame: %s", result.errFrame.GetMessage())
	}
	if accountID := <-accountIDs; accountID != "acct-from-jwt" {
		t.Fatalf("account id = %q, want acct-from-jwt", accountID)
	}
}

func headerValue(headers map[string]*pluginv1.HeaderValues, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values.GetValues()) > 0 {
			return values.GetValues()[0]
		}
	}
	return ""
}

func sseUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_sse\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
	}))
}

func TestForwardReplaySSEAsSyntheticEvents(t *testing.T) {
	upstream := sseUpstream(t)
	defer upstream.Close()

	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(`{"model":"gpt-6-astra","input":[],"stream":true}`)))
	if result.errFrame != nil {
		t.Fatalf("unexpected error frame: %s", result.errFrame.GetMessage())
	}
	stream := string(result.body)
	if !strings.Contains(stream, "event: response.completed") || !strings.HasSuffix(stream, "data: [DONE]\n\n") {
		t.Fatalf("synthetic stream = %s", stream)
	}
	if got := headerValue(result.headers, "Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", got)
	}
}

func TestForwardStreamsFirstSSEEventBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("test server does not support flushing")
		}
		_, _ = fmt.Fprint(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_first\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
		flusher.Flush()
		select {
		case <-release:
		case <-time.After(2 * time.Second):
			t.Error("forwarder did not receive the first flushed event")
		}
		_, _ = fmt.Fprint(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_first\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	transport := New()
	defer transport.Shutdown()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	stub := &timingStreamStub{streamStub: streamStub{ctx: context.Background(), requests: requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(`{"model":"gpt-6-astra","input":[],"stream":true}`))}, firstBodyChunk: make(chan time.Time, 1)}
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- transport.Forward(stub) }()
	select {
	case <-stub.firstBodyChunk:
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("first SSE event arrived too late: %v", elapsed)
		}
		close(release)
	case <-time.After(1500 * time.Millisecond):
		close(release)
		t.Fatal("first SSE event was buffered until upstream completion")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Forward returned transport error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Forward did not finish after upstream release")
	}
}

func TestForwardRelaysTextAndConvertsNativeToolInStream(t *testing.T) {
	native := map[string]any{
		"type":      "function_call",
		"id":        "fc_native_weather",
		"call_id":   "call_native_weather",
		"name":      "run_officejs",
		"status":    "completed",
		"arguments": string(protocol.JSONBytes(map[string]any{"summary": "Get weather", "code": string(protocol.JSONBytes(map[string]any{"tool": "get_weather", "args": map[string]any{"city": "Tokyo"}}))})),
	}
	completed := map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_tool", "status": "completed", "output": []any{native}}}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeEvent := func(event string, payload any) {
			_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, protocol.JSONBytes(payload))
		}
		writeEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_tool", "status": "in_progress", "output": []any{}}})
		writeEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native})
		writeEvent("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": "fc_native_weather", "arguments": native["arguments"]})
		writeEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native})
		writeEvent("response.completed", completed)
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	transport := New()
	defer transport.Shutdown()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	body := []byte(`{"model":"gpt-6-astra","input":[],"stream":true,"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`)
	result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, body))
	if result.errFrame != nil {
		t.Fatalf("unexpected error frame: %s", result.errFrame.GetMessage())
	}
	stream := string(result.body)
	if strings.Contains(stream, "run_officejs") || !strings.Contains(stream, "get_weather") || !strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("stream tool conversion = %s", stream)
	}
}

// Basis Points 的 Excel 网关对 Responses 请求总是回 SSE；下游没要求流式时，
// 插件必须把 SSE 还原成 JSON，否则客户端会拿到解析不了的流。
func TestForwardConvertsUpstreamSSEToJSONWhenClientWantsJSON(t *testing.T) {
	upstream := sseUpstream(t)
	defer upstream.Close()

	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(`{"model":"gpt-6-astra","input":[]}`)))
	if result.errFrame != nil {
		t.Fatalf("unexpected error frame: %s", result.errFrame.GetMessage())
	}
	if strings.Contains(string(result.body), "event: ") {
		t.Fatalf("non-stream client received SSE: %s", result.body)
	}
	var response map[string]any
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatalf("non-stream body is not JSON: %v (%s)", err, result.body)
	}
	if response["id"] != "resp_sse" || response["status"] != "completed" {
		t.Fatalf("converted response = %#v", response)
	}
	if got := headerValue(result.headers, "Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content type = %q, want application/json", got)
	}
}

func TestForwardPassesThroughUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = writer.Write([]byte(`{"detail":"unsupported reasoning effort"}`))
	}))
	defer upstream.Close()

	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(`{"model":"gpt-6-astra","input":[]}`)))
	if result.errFrame != nil {
		t.Fatalf("error frame should not be used for an upstream HTTP response: %s", result.errFrame.GetMessage())
	}
	if result.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", result.status)
	}
	if !strings.Contains(string(result.body), "unsupported reasoning effort") {
		t.Fatalf("upstream body was not passed through: %s", result.body)
	}
}

// 既没有宿主凭据、又没有宿主反向服务可以解析：这类失败换账号也不会变好，
// 必须按不可重放上报，否则宿主会把账号池挨个试穿再抛出一个与原因无关的 503。
func TestForwardFailsBeforeUpstreamWhenCredentialsAreMissing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("upstream must not be contacted")
	}))
	defer upstream.Close()

	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", "", nil, []byte(`{"model":"gpt-6-astra","input":[]}`)))
	if result.errFrame == nil {
		t.Fatal("missing credentials were accepted")
	}
	if !result.errFrame.GetRequestSent() {
		t.Fatal("a failure no account switch can fix must be marked unreplayable")
	}
	if !strings.Contains(result.errFrame.GetMessage(), "host services are unavailable") {
		t.Fatalf("unexpected message: %s", result.errFrame.GetMessage())
	}
}

func TestForwardRejectsUnsupportedPlatformAndProxy(t *testing.T) {
	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": "https://bps.example.test/basispoints/api/responses"})

	frames := requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, nil)
	frames[0].GetStart().Platform = "anthropic"
	result := runForward(t, transport, frames)
	if result.errFrame == nil || result.errFrame.GetRequestSent() {
		t.Fatalf("unsupported platform was not rejected early: %+v", result.errFrame)
	}

	frames = requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, nil)
	frames[0].GetStart().ProxyUrl = "ftp://proxy.example.test"
	result = runForward(t, transport, frames)
	if result.errFrame == nil || result.errFrame.GetRequestSent() {
		t.Fatalf("invalid proxy was not rejected early: %+v", result.errFrame)
	}
}

func TestForwardRejectsMissingBodyEnd(t *testing.T) {
	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": "https://bps.example.test/basispoints/api/responses"})
	frames := requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "acct"), nil, []byte(`{}`))
	stub := &streamStub{ctx: context.Background(), requests: frames[:len(frames)-1]}
	if err := transport.Forward(stub); err != nil {
		t.Fatalf("Forward returned transport error: %v", err)
	}
	if len(stub.responses) != 1 || stub.responses[0].GetError() == nil {
		t.Fatalf("incomplete request frames were accepted: %#v", stub.responses)
	}
}

func TestTestConfigProbesReachabilityWithoutCredentials(t *testing.T) {
	transport := New()
	reachable, err := transport.TestConfig(context.Background(), &pluginv1.TestConfigRequest{
		ConfigJson: []byte(`{"responses_url":"https://127.0.0.1:1/basispoints/api/responses"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if reachable.GetSuccess() {
		t.Fatal("unroutable host was reported reachable")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	defer upstream.Close()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	ok, err := transport.TestConfig(context.Background(), &pluginv1.TestConfigRequest{
		ConfigJson: []byte(`{"responses_url":"` + upstream.URL + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok.GetSuccess() {
		t.Fatalf("local endpoint reported unreachable: %s", ok.GetMessage())
	}
}

// fakeHost 只实现账号目录相关的两个 RPC，其余宿主服务方法保持未实现。
type fakeHost struct {
	pluginv1.HostServiceClient
	resolvedAccountIDs []int64
	accounts           []*pluginv1.AccountInfo
	token              string
	tokenFor           map[int64]string
	proxyURLFor        map[int64]string
	headers            map[string]*pluginv1.HeaderValues
	listErr            error
	resolveErr         error
}

func (f *fakeHost) ListAccounts(context.Context, *pluginv1.ListAccountsRequest, ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &pluginv1.ListAccountsResponse{Accounts: f.accounts}, nil
}

func (f *fakeHost) ResolveOutboundIdentity(_ context.Context, in *pluginv1.ResolveOutboundIdentityRequest, _ ...grpc.CallOption) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	f.resolvedAccountIDs = append(f.resolvedAccountIDs, in.GetAccountId())
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	token := f.token
	if mapped, ok := f.tokenFor[in.GetAccountId()]; ok {
		token = mapped
	}
	proxyURL := ""
	if mapped, ok := f.proxyURLFor[in.GetAccountId()]; ok {
		proxyURL = mapped
	}
	return &pluginv1.ResolveOutboundIdentityResponse{
		Found:     true,
		AccountId: in.GetAccountId(),
		Token:     token,
		ProxyUrl:  proxyURL,
		Headers:   f.headers,
	}, nil
}

// 宿主只负责调度、没给出站令牌时，插件按【同一个账号】解析凭据与代理 ——
// 不会拿别的账号的令牌或代理去替换它。
func TestPrepareHeadersUsesResolvedAccountCredentialAndProxy(t *testing.T) {
	transport := New()
	host := &fakeHost{
		tokenFor:    map[int64]string{42: token(t, "acct-42")},
		proxyURLFor: map[int64]string{42: "http://account-42-proxy.example.test:8080"},
		headers: map[string]*pluginv1.HeaderValues{
			"chatgpt-account-id": {Values: []string{"acct-42"}},
		},
	}
	start := &pluginv1.ForwardRequestStart{
		AccountId: 42,
		ProxyUrl:  "http://account-42-proxy.example.test:8080",
	}
	gotHeaders, gotProxy, err := transport.prepareHeaders(context.Background(), start, host)
	if err != nil {
		t.Fatalf("prepareHeaders failed: %v", err)
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer "+token(t, "acct-42") {
		t.Fatalf("authorization = %q, want the resolved account token", got)
	}
	if gotProxy != "http://account-42-proxy.example.test:8080" {
		t.Fatalf("proxy = %q, want the same account's proxy", gotProxy)
	}
	if len(host.resolvedAccountIDs) != 1 || host.resolvedAccountIDs[0] != 42 {
		t.Fatalf("resolved accounts = %#v, want [42]", host.resolvedAccountIDs)
	}
}

func TestPrepareHeadersKeepsScheduledAccountProxy(t *testing.T) {
	transport := New()
	start := &pluginv1.ForwardRequestStart{
		AccountId: 7,
		ProxyUrl:  "http://scheduled-proxy.example.test:8080",
		Headers: map[string]*pluginv1.HeaderValues{
			"Authorization":      {Values: []string{"Bearer scheduled-token"}},
			"ChatGPT-Account-Id": {Values: []string{"acct-scheduled"}},
		},
	}
	_, gotProxy, err := transport.prepareHeaders(context.Background(), start, nil)
	if err != nil {
		t.Fatalf("prepareHeaders failed: %v", err)
	}
	if gotProxy != start.GetProxyUrl() {
		t.Fatalf("proxy = %q, want scheduled account proxy %q", gotProxy, start.GetProxyUrl())
	}
}

func TestClientForProxyReusesCachedTransportConcurrently(t *testing.T) {
	transport := New()
	const workers = 32
	clients := make(chan *http.Client, workers)
	for i := 0; i < workers; i++ {
		go func() {
			client, err := transport.clientForProxy(transport.client, "http://proxy.example.test:8080")
			if err != nil {
				clients <- nil
				return
			}
			clients <- client
		}()
	}
	var first *http.Client
	for i := 0; i < workers; i++ {
		client := <-clients
		if client == nil {
			t.Fatal("clientForProxy returned nil")
		}
		if first == nil {
			first = client
		} else if client != first {
			t.Fatal("concurrent requests did not share the cached proxy client")
		}
	}
	if len(transport.proxyClients) != 1 {
		t.Fatalf("cached proxy clients = %d, want 1", len(transport.proxyClients))
	}
	transport.closeProxyClients()
	if len(transport.proxyClients) != 0 {
		t.Fatalf("proxy cache was not cleared: %d", len(transport.proxyClients))
	}
}

func TestClientForProxyDoesNotRepopulateCacheWithStaleBase(t *testing.T) {
	transport := New()
	oldBase := transport.client
	oldClient, err := transport.clientForProxy(oldBase, "http://proxy.example.test:8080")
	if err != nil {
		t.Fatalf("initial proxy client: %v", err)
	}
	if oldClient == nil {
		t.Fatal("initial proxy client is nil")
	}
	applyConfig(t, transport, map[string]any{"timeout_seconds": 301})
	newBase := transport.client
	if newBase == oldBase {
		t.Fatal("ApplyConfig did not replace the base client")
	}

	// Simulate a request that snapshotted oldBase before ApplyConfig completed.
	got, err := transport.clientForProxy(oldBase, "http://proxy.example.test:8080")
	if err != nil {
		t.Fatalf("stale request proxy client: %v", err)
	}
	if got == oldClient {
		t.Fatal("stale base client was reused after ApplyConfig")
	}
	entry := transport.proxyClients["http://proxy.example.test:8080"]
	if entry.base != newBase {
		t.Fatalf("proxy cache base = %p, want active base %p", entry.base, newBase)
	}
}

func TestForwardUsesPinnedAccountCredential(t *testing.T) {
	captured := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured <- request.Header.Clone()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	host := &fakeHost{
		token: "pinned-access-token",
		headers: map[string]*pluginv1.HeaderValues{
			"chatgpt-account-id": {Values: []string{"acct-pinned"}},
		},
	}
	transport := New()
	transport.host = host
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL, "account_ids": []int64{42}})

	// 宿主调度账号 42、但没带 Authorization：插件解析【同一个账号】的凭据。
	// 宿主透传的账号头保持权威（影子账号会刻意透传母账号 ID），只补空缺。
	frames := requestFrames(t, "https://ignored.example.test/v1/responses", "", map[string]string{
		"Chatgpt-Account-Id": "acct-scheduled",
		"Accept":             "application/json",
	}, []byte(`{"model":"gpt-6-astra","input":[]}`))
	frames[0].GetStart().AccountId = 42
	result := runForward(t, transport, frames)
	if result.errFrame != nil {
		t.Fatalf("unexpected error frame: %s", result.errFrame.GetMessage())
	}
	headers := <-captured
	if headers.Get("Authorization") != "Bearer pinned-access-token" {
		t.Fatalf("authorization = %q, want the resolved account token", headers.Get("Authorization"))
	}
	if headers.Get("ChatGPT-Account-ID") != "acct-scheduled" {
		t.Fatalf("account id = %q, want the host-passed value to stay authoritative", headers.Get("ChatGPT-Account-ID"))
	}
	if len(host.resolvedAccountIDs) != 1 || host.resolvedAccountIDs[0] != 42 {
		t.Fatalf("resolved accounts = %#v, want [42]", host.resolvedAccountIDs)
	}
}

// 固定账号解析不出凭据时，宿主换账号也无济于事 —— 插件永远只用自己解析出来的
// 那组账号。这类失败必须上报 request_sent=true，否则宿主会把整个账号池挨个试穿，
// 再把池子耗空后回一个与真实原因无关的 "no available accounts" 503。
func TestForwardFailsBeforeUpstreamWhenPinnedAccountIsUnresolvable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("upstream must not be contacted")
	}))
	defer upstream.Close()

	transport := New() // 没有宿主服务：无法解析白名单账号（账号 9 在白名单里，必须走解析）
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL, "account_ids": []int64{9}})
	frames := requestFrames(t, "https://ignored.example.test/v1/responses", "", nil, []byte(`{"model":"gpt-6-astra","input":[]}`))
	frames[0].GetStart().AccountId = 9
	result := runForward(t, transport, frames)
	if result.errFrame == nil {
		t.Fatal("pinned account without host services was accepted")
	}
	if !result.errFrame.GetRequestSent() {
		t.Fatal("a pinned-account failure must not invite account failover")
	}
	if !strings.Contains(result.errFrame.GetMessage(), "cannot resolve account") {
		t.Fatalf("error must name the real cause instead of a generic transport failure: %s", result.errFrame.GetMessage())
	}

	// 宿主解析接口本身报错属于瞬时故障：保持可重放，换一个账号可能就解析得出来。
	transport.host = &fakeHost{resolveErr: context.DeadlineExceeded}
	frames = requestFrames(t, "https://ignored.example.test/v1/responses", "", nil, []byte(`{"model":"gpt-6-astra","input":[]}`))
	frames[0].GetStart().AccountId = 9
	result = runForward(t, transport, frames)
	if result.errFrame == nil {
		t.Fatal("host identity lookup failure was accepted")
	}
	if result.errFrame.GetRequestSent() {
		t.Fatal("a transient host lookup failure must stay replayable on another account")
	}
}

// 跟随宿主调度时，宿主为本次请求选定的账号解析失败，换一个账号可能成功，
// 因此必须保持可重放（request_sent=false）。
func TestForwardKeepsScheduledIdentityFailureReplayable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("upstream must not be contacted")
	}))
	defer upstream.Close()

	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL})
	transport.host = &fakeHost{resolveErr: context.DeadlineExceeded}
	result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", "", nil, []byte(`{"model":"gpt-6-astra","input":[]}`)))
	if result.errFrame == nil {
		t.Fatal("missing host identity was accepted")
	}
	if result.errFrame.GetRequestSent() {
		t.Fatal("scheduled-account resolution failure must stay replayable on another account")
	}
}

func TestHealthExposesAccountDirectory(t *testing.T) {
	transport := New()
	transport.host = &fakeHost{accounts: []*pluginv1.AccountInfo{
		{Id: 1, Name: "primary", Status: "active", Schedulable: true},
		{Id: 2, Name: "paused", Status: "active", Schedulable: false},
	}}
	health, err := transport.Health(context.Background(), &pluginv1.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !health.GetHealthy() {
		t.Fatalf("plugin reported unhealthy: %s", health.GetMessage())
	}
	var status struct {
		AccountIDs []int64          `json:"account_ids"`
		Accounts   []accountSummary `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(health.GetStatusJson()), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.AccountIDs) != 0 {
		t.Fatalf("account_ids = %#v, want empty", status.AccountIDs)
	}
	if len(status.Accounts) != 2 {
		t.Fatalf("accounts = %#v", status.Accounts)
	}
	if status.Accounts[0].ID != 1 || status.Accounts[0].Name != "primary" || !status.Accounts[0].Schedulable {
		t.Fatalf("first account = %#v", status.Accounts[0])
	}
	if status.Accounts[1].Schedulable {
		t.Fatalf("paused account should be reported as not schedulable: %#v", status.Accounts[1])
	}
}

func TestHealthToleratesAccountDirectoryFailure(t *testing.T) {
	transport := New()
	transport.host = &fakeHost{listErr: context.DeadlineExceeded}
	health, err := transport.Health(context.Background(), &pluginv1.HealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !health.GetHealthy() {
		t.Fatal("account directory failure must not affect health")
	}
	if !strings.Contains(health.GetStatusJson(), `"accounts":[]`) {
		t.Fatalf("status = %s", health.GetStatusJson())
	}
}

// 插件不做任何账号轮询：每次请求都用宿主本次调度的那个账号的凭据与代理。
func TestForwardAlwaysUsesTheScheduledAccountCredential(t *testing.T) {
	captured := make(chan string, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured <- request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	host := &fakeHost{
		tokenFor: map[int64]string{1: "token-1", 2: "token-2", 3: "token-3"},
		// 宿主解析身份时总会带上账号头；缺失会让插件无法构造 chatgpt-account-id。
		headers: map[string]*pluginv1.HeaderValues{
			"chatgpt-account-id": {Values: []string{"acct-scheduled"}},
		},
	}
	transport := New()
	transport.host = host
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL, "account_ids": []int64{1, 2, 3}})

	for i := 0; i < 3; i++ {
		frames := requestFrames(t, "https://ignored.example.test/v1/responses", "", nil, []byte(`{"model":"gpt-6-astra","input":[]}`))
		frames[0].GetStart().AccountId = 1
		result := runForward(t, transport, frames)
		if result.errFrame != nil {
			t.Fatalf("unexpected error frame: %s", result.errFrame.GetMessage())
		}
		if got := <-captured; got != "Bearer token-1" {
			t.Fatalf("authorization = %q, want the scheduled account token", got)
		}
	}
	got := host.resolvedAccountIDs
	if len(got) != 3 {
		t.Fatalf("resolved accounts = %#v, want three lookups", got)
	}
	for _, id := range got {
		if id != 1 {
			t.Fatalf("resolved account = %d, want the scheduled account 1", id)
		}
	}
}

// 宿主本次调度的账号若已在勾选列表内：直接沿用宿主凭据，不再额外解析。
// 账号白名单的语义：勾选 = "这些账号走 Basis Points"，未勾选 = "不改动它们"。
// 宿主把请求交给插件后插件无法拒绝接管，所以白名单外的账号必须被原样透传回
// 宿主指定的上游 —— 对客户端等同于没有启用插件，不注入任何 Basis Points 特征。
func TestForwardPassesThroughAccountsOutsideTheWhitelist(t *testing.T) {
	var captured *http.Request
	var capturedBody []byte
	hostUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured = request.Clone(context.Background())
		capturedBody, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer hostUpstream.Close()

	basisPoints := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("Basis Points endpoint must not be used for an account outside the whitelist")
	}))
	defer basisPoints.Close()

	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": basisPoints.URL, "account_ids": []int64{42}})

	body := []byte(`{"model":"gpt-6-astra","input":"hi"}`)
	hostToken := token(t, "acct-scheduled")
	frames := requestFrames(t, hostUpstream.URL, hostToken, map[string]string{
		"Openai-Beta": "responses=v1",
		"Accept":      "text/event-stream",
	}, body)
	frames[0].GetStart().AccountId = 7 // 不在白名单里
	result := runForward(t, transport, frames)
	if result.errFrame != nil {
		t.Fatalf("passthrough failed: %s", result.errFrame.GetMessage())
	}
	if result.status != http.StatusOK {
		t.Fatalf("passthrough status = %d, want 200", result.status)
	}
	if captured == nil {
		t.Fatal("the host upstream was never contacted")
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer "+hostToken {
		t.Fatalf("authorization = %q, want the host credential untouched", got)
	}
	if captured.Header.Get("Openai-Beta") != "responses=v1" {
		t.Fatal("host request headers were dropped during passthrough")
	}
	if captured.Header.Get("X-Basispoints-Auth-Mode") != "" || captured.Header.Get("ChatGPT-Account-ID") != "" {
		t.Fatal("Basis Points headers leaked into a passthrough request")
	}
	if string(capturedBody) != string(body) {
		t.Fatalf("request body was rewritten: %s", capturedBody)
	}
	if !strings.Contains(string(result.body), "response.completed") {
		t.Fatalf("upstream response was not relayed verbatim: %s", result.body)
	}
}

// 白名单外的账号如果宿主没有给出上游 URL，就只能报错，不能猜一个地址发出去。
func TestForwardRejectsPassthroughWithoutHostUpstreamURL(t *testing.T) {
	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": "https://ignored.example.test/responses", "account_ids": []int64{42}})
	frames := requestFrames(t, "", token(t, "acct"), nil, []byte(`{"model":"gpt-6-astra","input":"hi"}`))
	frames[0].GetStart().AccountId = 7
	result := runForward(t, transport, frames)
	if result.errFrame == nil {
		t.Fatal("passthrough without a host upstream URL was accepted")
	}
	if result.errFrame.GetCode() != "invalid_request" || result.errFrame.GetRequestSent() {
		t.Fatalf("unexpected error frame: %+v", result.errFrame)
	}
}

// 透传路径必须走宿主为该账号选定的代理，不能悄悄直连。
// （空代理才代表直连 —— 宿主没给代理时不继承进程环境变量。）
func TestForwardPassthroughUsesTheHostAccountProxy(t *testing.T) {
	var proxied int32
	var proxiedURL string
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&proxied, 1)
		proxiedURL = request.URL.String()
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer proxyServer.Close()

	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": "https://ignored.example.test/responses", "account_ids": []int64{42}})

	body := []byte(`{"model":"gpt-6-astra","input":"hi"}`)
	frames := requestFrames(t, "http://upstream.example.test/responses", token(t, "acct"), nil, body)
	frames[0].GetStart().AccountId = 7 // 不在白名单 → 透传
	frames[0].GetStart().ProxyUrl = proxyServer.URL

	result := runForward(t, transport, frames)
	if result.errFrame != nil {
		t.Fatalf("passthrough through the account proxy failed: %s", result.errFrame.GetMessage())
	}
	if atomic.LoadInt32(&proxied) == 0 {
		t.Fatal("the host-selected account proxy was bypassed during passthrough")
	}
	if proxiedURL != "http://upstream.example.test/responses" {
		t.Fatalf("proxy saw %q, want the host upstream URL", proxiedURL)
	}
	if !strings.Contains(string(result.body), "response.completed") {
		t.Fatalf("proxied response was not relayed: %s", result.body)
	}
}

// 白名单内账号走 BPS 时，凭据与代理必须配套：解析出的账号代理要覆盖宿主为
// 另一个账号选的代理（否则会出现"用 A 的 token 从 B 的出口发出去"）。
func TestForwardBusinessPathKeepsCredentialAndProxyPaired(t *testing.T) {
	var proxied int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&proxied, 1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer proxyServer.Close()

	host := &fakeHost{
		token:       "pinned-access-token",
		proxyURLFor: map[int64]string{42: proxyServer.URL},
		headers:     map[string]*pluginv1.HeaderValues{"chatgpt-account-id": {Values: []string{"acct-pinned"}}},
	}
	transport := New()
	transport.host = host
	applyConfig(t, transport, map[string]any{"responses_url": "http://basispoints.example.test/responses", "account_ids": []int64{42}})

	frames := requestFrames(t, "http://upstream.example.test/responses", "", nil, []byte(`{"model":"gpt-6-astra","input":"hi"}`))
	frames[0].GetStart().AccountId = 42 // 在白名单里 → 走 BPS
	// 宿主为别的账号选了一个代理；插件解析出的账号带自己的代理，应当覆盖它。
	frames[0].GetStart().ProxyUrl = "http://127.0.0.1:1"

	_ = runForward(t, transport, frames)
	if atomic.LoadInt32(&proxied) == 0 {
		t.Fatal("the resolved account's own proxy was not used for the Basis Points request")
	}
}

// 宿主用普通 Codex 模型（默认 gpt-5.4）对账号做连通性测试，这个请求也会被交给
// 插件。它必须原样透传回宿主的上游：打到 Basis Points 会被 403
// basispoints_model_access_changed 拒绝，宿主随即把账号判成异常/限流。
func TestForwardPassesThroughModelsThePluginDoesNotServe(t *testing.T) {
	var hostUpstreamHits int32
	hostUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&hostUpstreamHits, 1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer hostUpstream.Close()

	basisPoints := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("a model the plugin does not serve must never reach Basis Points")
	}))
	defer basisPoints.Close()

	transport := New()
	applyConfig(t, transport, map[string]any{"responses_url": basisPoints.URL})

	// 与宿主 createOpenAITestPayload 等价的形状。
	body := []byte(`{"model":"gpt-5.4","stream":true,"store":false,"instructions":"You are a helpful assistant.","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	frames := requestFrames(t, hostUpstream.URL, token(t, "acct"), nil, body)
	result := runForward(t, transport, frames)
	if result.errFrame != nil {
		t.Fatalf("the account-test request failed: %s", result.errFrame.GetMessage())
	}
	if got := atomic.LoadInt32(&hostUpstreamHits); got != 1 {
		t.Fatalf("host upstream hits = %d, want the request passed through", got)
	}
	if !strings.Contains(string(result.body), "response.completed") {
		t.Fatalf("passthrough body = %s", result.body)
	}

	// 反向确认：唯一受支持的模型仍然走 Basis Points。
	for _, model := range []string{"gpt-6-astra"} {
		served := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: " + `{"type":"response.completed","response":{"id":"resp_served","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}}` + "\n\ndata: [DONE]\n\n"))
		}))
		transportServed := New()
		applyConfig(t, transportServed, map[string]any{"responses_url": served.URL})
		frames := requestFrames(t, hostUpstream.URL, token(t, "acct"), nil, []byte(`{"model":"`+model+`","input":"hi"}`))
		servedResult := runForward(t, transportServed, frames)
		if servedResult.errFrame != nil {
			t.Fatalf("%s was not served by the plugin: %s", model, servedResult.errFrame.GetMessage())
		}
		served.Close()
	}
}

func TestForwardKeepsScheduledAccountWhenListed(t *testing.T) {
	captured := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured <- request.Header.Clone()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	host := &fakeHost{token: "resolved-token"}
	transport := New()
	transport.host = host
	applyConfig(t, transport, map[string]any{"responses_url": upstream.URL, "account_ids": []int64{7, 9}})

	// requestFrames 默认 start.AccountId = 7，并在请求头里带了该账号的 Bearer 令牌。
	result := runForward(t, transport, requestFrames(t, "https://ignored.example.test/v1/responses", token(t, "scheduled"), nil, []byte(`{"model":"gpt-6-astra","input":[]}`)))
	if result.errFrame != nil {
		t.Fatalf("unexpected error frame: %s", result.errFrame.GetMessage())
	}
	if len(host.resolvedAccountIDs) != 0 {
		t.Fatalf("host was consulted unnecessarily: %#v", host.resolvedAccountIDs)
	}
	headers := <-captured
	if headers.Get("Authorization") != "Bearer "+token(t, "scheduled") {
		t.Fatalf("scheduled account credential was replaced: %q", headers.Get("Authorization"))
	}
}
