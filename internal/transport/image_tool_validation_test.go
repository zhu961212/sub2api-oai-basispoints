package transport

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestToolImageValidationCannotBypassDisabledConversions(t *testing.T) {
	_, imageURL := relayTestImage(t)
	imagePayload := strings.SplitN(imageURL, ",", 2)[1]
	tests := []struct {
		name   string
		code   string
		mutate func(item, image map[string]any)
	}{
		{"url_leading_space", "unsupported_capability", func(item, image map[string]any) { image["image_url"] = " " + imageURL }},
		{"url_trailing_space", "unsupported_capability", func(item, image map[string]any) { image["image_url"] = imageURL + " " }},
		{"tool_type_leading_space", "unsupported_capability", func(item, image map[string]any) { item["type"] = " " + item["type"].(string) }},
		{"tool_type_trailing_space", "unsupported_capability", func(item, image map[string]any) { item["type"] = item["type"].(string) + " " }},
		{"tool_type_uppercase", "unsupported_capability", func(item, image map[string]any) { item["type"] = strings.ToUpper(item["type"].(string)) }},
		{"image_type_leading_space", "unsupported_capability", func(item, image map[string]any) { image["type"] = " input_image" }},
		{"image_type_trailing_space", "unsupported_capability", func(item, image map[string]any) { image["type"] = "input_image " }},
		{"image_type_uppercase", "unsupported_capability", func(item, image map[string]any) { image["type"] = "INPUT_IMAGE" }},
		{"invalid_base64", "invalid_image", func(item, image map[string]any) {
			image["image_url"] = "data:image/png;base64,PRIVATE_TOOL_SCREENSHOT_%%%%"
		}},
	}
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		for _, rewrite := range []bool{false, true} {
			for _, transform := range []bool{false, true} {
				for _, test := range tests {
					t.Run(fmt.Sprintf("%s/rewrite=%t/transform=%t/%s", kind, rewrite, transform, test.name), func(t *testing.T) {
						var calls atomic.Int32
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							w.WriteHeader(http.StatusInternalServerError)
						}))
						defer upstream.Close()
						tr := New()
						defer tr.Shutdown()
						applyConfig(t, tr, map[string]any{"responses_url": upstream.URL + "/responses", "rewrite_tools": rewrite, "transform_responses": transform})
						image := map[string]any{"type": "input_image", "image_url": imageURL, "detail": nil}
						item := map[string]any{"type": kind, "call_id": "call_private_screenshot", "output": []any{image}}
						test.mutate(item, image)
						source := map[string]any{"model": protocol.DefaultModelID, "input": []any{item}}
						result := runForward(t, tr, requestFrames(t, upstream.URL+"/responses", token(t, "acct-image-validation"), nil, protocol.JSONBytes(source)))
						if result.errFrame != nil || result.status != http.StatusBadRequest || !result.ended || result.received != int64(len(result.body)) {
							t.Fatalf("invalid rejection framing: status=%d error=%v ended=%t", result.status, result.errFrame, result.ended)
						}
						response, err := protocol.RawObject(result.body)
						if err != nil || relayObject(response["error"])["code"] != test.code {
							t.Fatalf("rejection did not return expected safe code %s", test.code)
						}
						if calls.Load() != 0 {
							t.Fatalf("invalid screenshot reached an attachment or conversation endpoint: calls=%d", calls.Load())
						}
						tr.imageAdmission.mu.Lock()
						requests, reserved := tr.imageAdmission.requests, tr.imageAdmission.bytes
						tr.imageAdmission.mu.Unlock()
						if requests != 0 || reserved != 0 {
							t.Fatalf("image rejection leaked admission budget: requests=%d bytes=%d", requests, reserved)
						}
						if bytes.Contains(result.body, []byte(imagePayload)) || bytes.Contains(result.body, []byte("PRIVATE_TOOL_SCREENSHOT")) || bytes.Contains(result.body, []byte("data:image/")) {
							t.Fatal("validation diagnostics exposed screenshot content")
						}
					})
				}
			}
		}
	}
}
