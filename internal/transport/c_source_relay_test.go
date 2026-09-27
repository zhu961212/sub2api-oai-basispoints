package transport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func cSourceRelayInput() string {
	patch := strings.Join([]string{
		"*** Begin Patch", "*** Add File: hello.c",
		"+#include <stdio.h>", "+#include \"local.h\"",
		"+int main(void) {",
		"+    const char *path = \"C:\\\\temp\\\\demo.c\";",
		"+    printf(\"%s: %d\\n\", path, 42);",
		"+    putchar('\\0'); /* \u4e2d\u6587: preserve source bytes */",
		"+    return 0;", "+}", "*** End Patch",
	}, string(byte(10)))
	return "  text(await tools.apply_patch(String.raw" + string(byte(96)) + patch + string(byte(96)) + "));  "
}

func TestCSourceRelayPreservesCodeAndBoundsCorrection(t *testing.T) {
	t.Run("tool_first", func(t *testing.T) { testCSourceRelayCorrection(t, false) })
	t.Run("args_first", func(t *testing.T) { testCSourceRelayCorrection(t, true) })
}

func testCSourceRelayCorrection(t *testing.T, argsFirst bool) {
	input := cSourceRelayInput()
	quote := string(byte(34))
	valid := "{" + quote + "tool" + quote + ":" + quote + "functions.exec" + quote + "," + quote + "args" + quote + ":" + string(protocol.JSONBytes(input)) + "}"
	if argsFirst {
		valid = "{" + quote + "args" + quote + ":" + string(protocol.JSONBytes(input)) + "," + quote + "tool" + quote + ":" + quote + "functions.exec" + quote + "}"
	}
	for _, damaged := range []bool{false, true} {
		for _, stillInvalid := range []bool{false, true} {
			if !damaged && stillInvalid {
				continue
			}
			for _, stream := range []bool{false, true} {
				for _, upstreamSSE := range []bool{false, true} {
					t.Run(fmt.Sprintf("damaged=%t/repair_invalid=%t/stream=%t/sse=%t", damaged, stillInvalid, stream, upstreamSSE), func(t *testing.T) {
						code := valid
						if damaged {
							code = strings.ReplaceAll(valid, string(byte(92))+quote, quote)
						}
						initial := relayNativeCall("call_c_original", "functions.exec", input)
						initial["arguments"] = string(protocol.JSONBytes(map[string]any{"summary": "write C source", "code": code}))
						var attempts atomic.Int32
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							_, _ = io.Copy(io.Discard, r.Body)
							call := initial
							if attempts.Add(1) > 1 && !stillInvalid {
								call = relayNativeCall("call_c_corrected", "functions.exec", input)
							}
							response := map[string]any{"id": "resp_c_source", "status": "completed", "output": []any{call}}
							if !upstreamSSE {
								w.Header().Set("Content-Type", "application/json")
								_, _ = w.Write(protocol.JSONBytes(response))
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_c_source", "status": "in_progress", "output": []any{}}})+streamData(map[string]any{"type": "response.completed", "response": response}))
						}))
						defer upstream.Close()
						tr := New()
						defer tr.Shutdown()
						applyConfig(t, tr, map[string]any{"responses_url": upstream.URL})
						source := executorOnlyCatalogSource(t.Name())
						source["stream"] = stream
						result := runForward(t, tr, requestFrames(t, "https://host.invalid/v1/responses", token(t, "acct-c-source"), nil, protocol.JSONBytes(source)))
						wantAttempts := int32(1)
						if damaged {
							wantAttempts = 2
						}
						if attempts.Load() != wantAttempts {
							t.Fatalf("attempts=%d, want %d", attempts.Load(), wantAttempts)
						}
						if stillInvalid {
							if streamFailureCode(result) != "invalid_tool_call" {
								t.Fatalf("invalid repair changed failure: %+v", result)
							}
							assertNoToolExecutionOnStreamError(t, result)
							return
						}
						if result.errFrame != nil || !result.ended {
							t.Fatalf("C source relay failed: %+v", result)
						}
						response := outerSyntaxCompletedResponse(t, result, input, stream)
						output, _ := response["output"].([]any)
						if len(output) != 1 {
							t.Fatalf("expected exactly one tool: %#v", response)
						}
						call := relayObject(output[0])
						wantID := "call_c_original"
						if damaged {
							wantID = "call_c_corrected"
						}
						if call["type"] != "custom_tool_call" || call["name"] != "functions.exec" || call["call_id"] != wantID || call["input"] != input {
							t.Fatalf("C source or identity changed: %#v", call)
						}
					})
				}
			}
		}
	}
}
