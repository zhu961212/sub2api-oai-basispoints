package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestForwardPreservesEnvironmentWithoutExternalLookup(t *testing.T) {
	for _, mode := range []string{"bps", "bps_tools", "native", "native_compact"} {
		for _, withEnvironment := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/environment=%t", mode, withEnvironment), func(t *testing.T) {
				tr := New()
				defer tr.Shutdown()
				const bpsURL = "https://fixture-bps.invalid/responses"
				applyConfig(t, tr, map[string]any{"responses_url": bpsURL, "native_timezone_by_ip": true, "rewrite_tools": mode == "bps_tools", "transform_responses": false})
				input := []any{}
				if withEnvironment {
					input = append(input, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<environment_context>\n  <cwd>/fixture</cwd>\n  <current_date>1999-01-01</current_date>\n  <timezone>Pacific/Kiritimati</timezone>\n</environment_context>"}}})
				}
				input = append(input, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Keep my original business prompt and date."}}})
				model, endpoint, expectedTarget := protocol.DefaultModelID, nativeDegradationResponsesURL, bpsURL
				if strings.HasPrefix(mode, "native") {
					model = "unselected-native-fixture"
					if mode == "native_compact" {
						endpoint += "/compact"
					}
					expectedTarget = endpoint
				}
				original := protocol.JSONBytes(map[string]any{"model": model, "input": input})
				calls := 0
				tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
					calls++
					if request.URL.String() != expectedTarget {
						t.Errorf("unexpected external lookup: %s", request.URL)
						return nil, fmt.Errorf("unexpected lookup")
					}
					raw, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					object, err := protocol.RawObject(raw)
					forwarded, _ := object["input"].([]any)
					// Tool bridging prepends its own developer instructions; the
					// complete original user input must still survive unchanged.
					if mode == "bps_tools" && len(forwarded) > 0 {
						first, _ := forwarded[0].(map[string]any)
						if first["role"] == "developer" {
							forwarded = forwarded[1:]
						}
					}
					if err != nil || !bytes.Equal(protocol.JSONBytes(forwarded), protocol.JSONBytes(input)) {
						t.Errorf("forwarding changed or injected date/timezone context: %s (%v)", raw, err)
					}
					if strings.HasPrefix(mode, "native") && !bytes.Equal(raw, original) {
						t.Error("native forwarding changed the original request bytes")
					}
					response := protocol.JSONBytes(map[string]any{"id": "resp_fixture", "status": "completed", "output": []any{}})
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(response))}, nil
				})}
				result := runForward(t, tr, requestFrames(t, endpoint, token(t, "environment-account"), nil, original))
				if result.status != http.StatusOK || result.errFrame != nil || !result.ended || calls != 1 {
					t.Fatalf("unexpected forwarding result: %+v, outbound calls=%d", result, calls)
				}
			})
		}
	}
}

func TestNativeProbePreservesPreparedBodyWithoutExternalLookup(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	applyConfig(t, tr, map[string]any{"native_timezone_by_ip": true})
	host := &fakeHost{tokenFor: map[int64]string{7: token(t, "probe-account")}}
	prepared, _, err := prepareNativeDegradationRequest(context.Background(), host, 7, nativeDegradationModel)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := io.ReadAll(prepared.Body)
	_ = prepared.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != nativeDegradationResponsesURL {
			t.Errorf("probe made an unexpected external lookup: %s", request.URL)
			return nil, fmt.Errorf("unexpected lookup")
		}
		raw, readErr := io.ReadAll(request.Body)
		if readErr != nil || !bytes.Equal(raw, expected) || bytes.Contains(raw, []byte("<environment_context>")) {
			t.Errorf("probe body changed after preparation: %s (%v)", raw, readErr)
		}
		response := protocol.JSONBytes(map[string]any{"output_text": "iPhone 17"})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(response))}, nil
	})}
	status, answer, err := tr.checkDegradationAccount(context.Background(), tr.cfg, host, tr.client, 7, nativeDegradationModel)
	if err != nil || status != "ok" || answer != "iPhone 17" || calls != 1 {
		t.Fatalf("probe = %s, %q, %v; outbound calls=%d", status, answer, err, calls)
	}
}
