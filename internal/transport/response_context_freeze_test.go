package transport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestResponseOnlyForwardFreezesCatalogBeforeConcurrentUpdates(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, inherit := range []bool{false, true} {
			for _, revoke := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/inherit=%t/revoke=%t", streaming, inherit, revoke), func(t *testing.T) {
					entered, release := make(chan struct{}), make(chan struct{})
					var once sync.Once
					unblock := func() { once.Do(func() { close(release) }) }
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						raw, _ := io.ReadAll(r.Body)
						if strings.Contains(string(raw), "__bps_") {
							t.Error("local frozen context leaked into raw upstream body")
						}
						source, err := protocol.RawObject(raw)
						if err != nil {
							t.Error(err)
							return
						}
						phase := protocol.StringValue(source["input"])
						output := []any{}
						switch phase {
						case "old":
							close(entered)
							select {
							case <-release:
							case <-r.Context().Done():
								return
							}
							output = []any{relayNativeCall("call_old", "old_tool", map[string]any{})}
						case "follow":
							tool := "new_tool"
							if revoke {
								tool = "old_tool"
							}
							output = []any{relayNativeCall("call_follow", tool, map[string]any{})}
						}
						response := map[string]any{"id": t.Name() + "/" + phase, "status": "completed", "output": output}
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, streamData(map[string]any{"type": "response.completed", "response": response}))
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write(protocol.JSONBytes(response))
						}
					}))
					defer upstream.Close()
					defer unblock()
					tr := New()
					defer tr.Shutdown()
					applyConfig(t, tr, map[string]any{"responses_url": upstream.URL, "rewrite_tools": false, "transform_responses": true})
					toolCatalog := func(name string) []any {
						return []any{map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object"}}}
					}
					forward := func(phase string, tools []any) forwardResult {
						source := map[string]any{"model": "gpt-6-astra", "stream": streaming, "input": phase}
						if tools != nil {
							source["tools"] = tools
						}
						return runForward(t, tr, requestFrames(t, upstream.URL, token(t, "acct"), map[string]string{"conversation_id": t.Name()}, protocol.JSONBytes(source)))
					}
					assertSuccess := func(result forwardResult) {
						t.Helper()
						if result.errFrame != nil || result.status != http.StatusOK || (streaming && streamFailureCode(result) != "") {
							t.Fatalf("response-only forwarding failed: error=%v body=%s", result.errFrame, result.body)
						}
					}
					oldCatalog := toolCatalog("old_tool")
					if inherit {
						assertSuccess(forward("seed", oldCatalog))
						oldCatalog = nil
					}
					oldResult := make(chan forwardResult, 1)
					go func() { oldResult <- forward("old", oldCatalog) }()
					select {
					case <-entered:
					case <-time.After(3 * time.Second):
						t.Fatal("older request did not reach upstream")
					}
					updated := toolCatalog("new_tool")
					if revoke {
						updated = []any{}
					}
					assertSuccess(forward("new", updated))
					unblock()
					select {
					case result := <-oldResult:
						assertSuccess(result)
						if !strings.Contains(string(result.body), "old_tool") {
							t.Fatalf("older request lost its frozen tool: %s", result.body)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("older request did not finish")
					}
					follow := forward("follow", nil)
					if !revoke {
						assertSuccess(follow)
						if !strings.Contains(string(follow.body), "new_tool") {
							t.Fatalf("late response replaced newer catalog: %s", follow.body)
						}
					} else if streaming {
						if streamFailureCode(follow) != "invalid_tool_call" {
							t.Fatalf("late response restored revoked tool: %s", follow.body)
						}
					} else if follow.errFrame == nil || follow.errFrame.GetCode() != "invalid_tool_call" {
						t.Fatalf("late response restored revoked tool: error=%v body=%s", follow.errFrame, follow.body)
					}
				})
			}
		}
	}
}
