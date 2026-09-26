package transport

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestTerminalContractMalformedFailureCannotResume(t *testing.T) {
	nl := string(rune(10))
	for _, first := range []string{"event: error", "event: response.failed" + nl + "data: not-json", "event: error" + nl + "data: [DONE]", "event: error" + nl + "data: "} {
		t.Run(first, func(t *testing.T) {
			native := relayNativeCall("call_after_malformed_failure", "get_weather", map[string]any{})
			completed := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{native}}})
			raw := first + nl + nl + completed
			result := runSSE(t, raw, streamToolRequest(t, "get_weather", "{}"))
			if strings.Contains(string(result.body), "response.completed") || strings.Contains(string(result.body), "function_call_arguments") || strings.Contains(string(result.body), "get_weather") {
				t.Fatalf("malformed failure resumed the live stream: %s", result.body)
			}
			upstream := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(raw))}
			if response, err := readToolRepairResponse(upstream, 1<<20); err == nil {
				t.Fatalf("malformed failure resumed tool repair: %v", response)
			}
		})
	}
}

func TestTerminalContractSyntheticFailureTerminal(t *testing.T) {
	for _, status := range []string{"failed", "incomplete", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			response := map[string]any{"id": "audit_failed", "status": status, "output": []any{}, "error": map[string]any{"code": "server_error", "message": "audit failure"}}
			raw, _, err := transformResponse(protocol.JSONBytes(response), http.Header{"Content-Type": {"application/json"}}, map[string]any{"stream": true})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "event: response.completed") {
				t.Fatalf("upstream %s rewritten as successful completion: %s", status, raw)
			}
		})
	}
}

func TestTerminalContractErrorEnvelopeMustNotReleaseTools(t *testing.T) {
	for _, location := range []string{"outer", "nested", "explicit403", "outer_missing_status"} {
		t.Run(location, func(t *testing.T) {
			native := relayNativeCall("call_audit_"+location, "get_weather", map[string]any{})
			response := map[string]any{"id": "audit_" + location, "status": "completed", "output": []any{native}}
			if location == "outer_missing_status" {
				delete(response, "status")
			}
			failure := map[string]any{"code": "server_error", "message": "audit failure"}
			payload := map[string]any{"type": "response.completed", "response": response}
			if location == "nested" {
				response["error"] = failure
			} else {
				payload["error"] = failure
			}
			if location == "explicit403" {
				failure["status_code"] = 403
			}
			raw := streamData(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native}) + streamData(payload)
			upstream := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(raw))}
			if err := prepareBasisPointsResponse(upstream, 1<<20); err != nil {
				t.Fatal(err)
			}
			source := map[string]any{"stream": true, "tools": []any{map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{"type": "object"}}}}
			stub := &streamStub{ctx: context.Background()}
			if err := sendTransformedHTTPResponseStreamWithKeepalive(stub, upstream, 1<<20, source, 0); err != nil {
				t.Fatal(err)
			}
			var emitted strings.Builder
			for _, frame := range stub.responses {
				emitted.Write(frame.GetBodyChunk())
			}
			if strings.Contains(emitted.String(), "response.function_call_arguments.done") {
				t.Fatalf("error envelope released executable tool: %s", emitted.String())
			}
		})
	}
}

func TestTerminalContractRepairOuterFailureMustBeRejected(t *testing.T) {
	source := executorOnlyCatalogSource(t.Name())
	original := map[string]any{"id": "audit_original", "status": "completed", "output": []any{relayNativeCall("audit_wrong", "exec_command", map[string]any{"cmd": "pwd"})}}
	repaired := map[string]any{"id": "audit_repair", "status": "completed", "output": []any{relayNativeCall("audit_fixed", "functions.exec", "text(1);")}}
	payload := map[string]any{"type": "response.completed", "error": map[string]any{"code": "server_error", "message": "audit outer failure"}, "response": repaired}
	upstream := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(streamData(payload)))}
	if err := prepareBasisPointsResponse(upstream, 1<<20); err != nil {
		t.Fatal(err)
	}
	got, err := readToolRepairResponse(upstream, 1<<20)
	if err != nil {
		return
	}
	merged, err := protocol.MergeToolRepairResponse(source, original, got)
	if err == nil {
		t.Fatalf("outer repair failure discarded and tool accepted: %s", protocol.JSONBytes(merged))
	}
}

func TestTerminalContractBufferedParserRequiresSuccessfulTerminal(t *testing.T) {
	for _, event := range []string{"response.created", "response.failed"} {
		t.Run(event, func(t *testing.T) {
			raw := []byte(streamData(map[string]any{"type": event, "response": map[string]any{"id": "audit_wrong_terminal", "status": "completed", "output": []any{}}}))
			response, err := protocol.ParseFinalStreamResponse(raw)
			if err == nil {
				t.Fatalf("noncompletion event accepted as completed: %s", protocol.JSONBytes(response))
			}
		})
	}
}

func TestTerminalContractBufferedParserCannotResumeAfterFailure(t *testing.T) {
	source := map[string]any{"stream": false, "tools": []any{map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{"type": "object"}}}}
	failure := streamData(map[string]any{"type": "response.failed", "response": map[string]any{"id": "audit_fail", "status": "failed", "output": []any{}, "error": map[string]any{"code": "server_error", "message": "audit failure"}}})
	success := streamData(map[string]any{"type": "response.completed", "response": map[string]any{"id": "audit_second_terminal", "status": "completed", "output": []any{relayNativeCall("audit_after_failure", "get_weather", map[string]any{})}}})
	got, _, err := transformResponse([]byte(failure+success), http.Header{"Content-Type": {"text/event-stream"}}, source)
	if err == nil && strings.Contains(string(got), "get_weather") {
		t.Fatalf("tool accepted after terminal failure: %s", got)
	}
}

func TestTerminalContractSSEErrorNameMustNotReleaseTools(t *testing.T) {
	native := relayNativeCall("call_audit_event_error", "get_weather", map[string]any{})
	payload := map[string]any{"type": "response.completed", "response": map[string]any{"id": "audit_event_error", "status": "completed", "output": []any{native}}}
	raw := "event: error" + string([]byte{10}) + streamData(payload)
	upstream := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(raw))}
	if err := prepareBasisPointsResponse(upstream, 1<<20); err != nil {
		t.Fatal(err)
	}
	source := map[string]any{"stream": true, "tools": []any{map[string]any{"type": "function", "name": "get_weather", "parameters": map[string]any{"type": "object"}}}}
	stub := &streamStub{ctx: context.Background()}
	if err := sendTransformedHTTPResponseStreamWithKeepalive(stub, upstream, 1<<20, source, 0); err != nil {
		return
	}
	var emitted strings.Builder
	for _, frame := range stub.responses {
		emitted.Write(frame.GetBodyChunk())
	}
	if strings.Contains(emitted.String(), "response.function_call_arguments.done") {
		t.Fatalf("SSE event:error released executable tool: %s", emitted.String())
	}
}

func TestTerminalContractBufferedFailurePreservesRequestScopedError(t *testing.T) {
	payload := map[string]any{"type": "response.failed", "response": map[string]any{"id": "audit_failed_only", "status": "failed", "output": []any{}, "error": map[string]any{"code": "insufficient_quota", "message": "audit quota failure"}}}
	upstream := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(streamData(payload)))}
	if err := prepareBasisPointsResponse(upstream, 1<<20); err != nil {
		t.Fatal(err)
	}
	raw, err := readLimited(upstream.Body, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := transformResponse(raw, upstream.Header, map[string]any{"stream": false})
	if api, ok := err.(*protocol.APIError); ok && api.Kind == "bps_service_rejected" {
		return
	}
	if err != nil {
		t.Fatalf("request-scoped failure lost after isolation: err=%v isolated=%s", err, raw)
	}
	if !strings.Contains(string(got), "bps_service_rejected") {
		t.Fatalf("request-scoped failure lost: %s", got)
	}
}
