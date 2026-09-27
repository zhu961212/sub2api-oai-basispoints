package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

// Reproduce a long client history whose final screenshot is input[267].output[2].
// All images are repeated deliberately: every occurrence still consumes budget.
func toolImageHistoryAt267(t *testing.T, imageURL string, custom bool, count int) map[string]any {
	t.Helper()
	tools, name, arguments := imageToolCatalog(custom)
	items := make([]any, 268)
	for index := range items {
		items[index] = map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "historical context"}}}
	}
	for index := 0; index < count; index++ {
		position := index * 2
		if index == count-1 {
			position = 266
		}
		id := fmt.Sprintf("call_image_history_%d", index)
		call := map[string]any{"type": "function_call", "name": name, "call_id": id, "arguments": string(protocol.JSONBytes(arguments))}
		outputType := "function_call_output"
		if custom {
			call = map[string]any{"type": "custom_tool_call", "name": name, "call_id": id, "input": arguments}
			outputType = "custom_tool_call_output"
		}
		parts := []any{map[string]any{"type": "input_text", "text": "screenshot follows"}, map[string]any{"type": "input_text", "text": "preserve original image"}, map[string]any{"type": "input_image", "image_url": imageURL, "detail": nil}}
		items[position], items[position+1] = call, map[string]any{"type": outputType, "call_id": id, "output": parts}
	}
	return map[string]any{"model": protocol.DefaultModelID, "tools": tools, "input": items}
}

func TestLongToolImageHistoryUsesBounded128ImageBudget(t *testing.T) {
	_, imageURL := relayTestImage(t)
	for _, custom := range []bool{false, true} {
		for _, rewrite := range []bool{false, true} {
			for _, transform := range []bool{false, true} {
				for _, count := range []int{21, 128, 129} {
					t.Run(fmt.Sprintf("custom=%t/rewrite=%t/transform=%t/count=%d", custom, rewrite, transform, count), func(t *testing.T) {
						var uploads, conversations atomic.Int32
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path != "/responses" {
								uploads.Add(1)
								http.Error(w, "tool images must remain inline", http.StatusBadRequest)
								return
							}
							conversations.Add(1)
							raw, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
							}
							source, err := protocol.RawObject(raw)
							if err != nil {
								t.Error(err)
								http.Error(w, "invalid JSON", http.StatusBadRequest)
								return
							}
							if message := imageToolWireError(source, imageURL); message != "" {
								t.Error(message)
							}
							if actual := len(relayTestFileIDs(source)); actual != count {
								t.Errorf("history image occurrences changed: got=%d want=%d", actual, count)
							}
							response, _ := protocol.RawObject(relayTestCompletedResponse())
							imageToolWriteResponse(w, response, false)
						}))
						defer upstream.Close()
						tr := New()
						defer tr.Shutdown()
						applyConfig(t, tr, map[string]any{"responses_url": upstream.URL + "/responses", "rewrite_tools": rewrite, "transform_responses": transform})
						source := toolImageHistoryAt267(t, imageURL, custom, count)
						before := protocol.JSONBytes(source)
						result := runForward(t, tr, requestFrames(t, upstream.URL+"/responses", token(t, "acct-long-image-history"), nil, before))
						if count <= 128 {
							imageToolClientResponse(t, result, false)
							if conversations.Load() != 1 {
								t.Fatalf("valid history was not sent exactly once: %d", conversations.Load())
							}
						} else {
							if result.status != http.StatusBadRequest || result.errFrame != nil || !result.ended || conversations.Load() != 0 {
								t.Fatalf("image limit rejection lost HTTP framing: status=%d err=%v calls=%d", result.status, result.errFrame, conversations.Load())
							}
							object, err := protocol.RawObject(result.body)
							if err != nil || relayObject(object["error"])["code"] != "invalid_image" {
								t.Fatalf("unexpected limit error: %s", result.body)
							}
							pathIndex := 267
							if rewrite {
								// Existing tool rewriting prepends one catalog message.
								pathIndex++
							}
							for _, fragment := range []string{"plugin allows at most 128", "received at least 129", fmt.Sprintf("path=input[%d].output[2]", pathIndex)} {
								if !strings.Contains(string(result.body), fragment) {
									t.Fatalf("missing bounded history diagnostic %q: %s", fragment, result.body)
								}
							}
						}
						if uploads.Load() != 0 || !bytes.Equal(before, protocol.JSONBytes(source)) {
							t.Fatal("history screenshot uploaded or client source mutated")
						}
					})
				}
			}
		}
	}
}
