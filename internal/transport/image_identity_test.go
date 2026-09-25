package transport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestImageReuploadsPreserveConversationAndTurnIdentity(t *testing.T) {
	for _, explicitSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-session=%t", explicitSession), func(t *testing.T) {
			imageBytes, inline := relayTestImage(t)
			uploads := 0
			var metadata []map[string]any
			var fileIDs []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/attachments" {
					uploads++
					relayTestUpload(t, w, r, imageBytes, fmt.Sprintf("file-renewed-%d", uploads))
					return
				}
				raw, _ := io.ReadAll(r.Body)
				request, err := protocol.RawObject(raw)
				if err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				metadata = append(metadata, request["metadata"].(map[string]any))
				ids := relayTestFileIDs(request)
				if len(ids) == 0 {
					t.Error("image was dropped")
				} else {
					fileIDs = append(fileIDs, ids[0])
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(relayTestCompletedResponse())
			}))
			defer upstream.Close()
			headers := map[string]string{}
			if explicitSession {
				headers["conversation_id"] = "trusted-image-conversation"
			}
			for round := 0; round < 3; round++ {
				source, _ := protocol.RawObject(relayTestBody(inline))
				input := source["input"].([]any)
				if round > 0 {
					input = append(input,
						map[string]any{"type": "function_call", "name": "inspect", "call_id": "call_1", "arguments": "{}"},
						map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"})
				}
				if round == 2 {
					input = append(input, map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "input_text", "text": "Now answer a new question about this image."},
						map[string]any{"type": "input_image", "image_url": inline}}})
				}
				source["input"] = input
				// A fresh uploader models an expired attachment cache or restart.
				tr := New()
				applyConfig(t, tr, map[string]any{"responses_url": upstream.URL + "/responses"})
				result := runForward(t, tr, requestFrames(t, "https://unused.invalid/responses", token(t, "acct-image-identity"), headers, protocol.JSONBytes(source)))
				tr.Shutdown()
				if result.errFrame != nil || result.status != 200 {
					t.Fatalf("round %d: status=%d error=%v", round, result.status, result.errFrame)
				}
			}
			if uploads != 3 || len(metadata) != 3 || len(fileIDs) != 3 || fileIDs[0] == fileIDs[1] {
				t.Fatalf("test did not exercise renewed attachment IDs: uploads=%d ids=%v", uploads, fileIDs)
			}
			if metadata[0]["task_id"] != metadata[1]["task_id"] || metadata[0]["turn_id"] != metadata[1]["turn_id"] {
				t.Fatalf("same conversation/turn changed when upload ID changed: %v", metadata)
			}
			if metadata[0]["agent_iteration"] != "1" || metadata[1]["agent_iteration"] != "2" {
				t.Fatalf("tool continuation lost its iteration: %v", metadata)
			}
			if metadata[0]["task_id"] != metadata[2]["task_id"] || metadata[0]["turn_id"] == metadata[2]["turn_id"] || metadata[2]["agent_iteration"] != "1" {
				t.Fatalf("new user turn did not retain task and start a new turn: %v", metadata)
			}
		})
	}
}
