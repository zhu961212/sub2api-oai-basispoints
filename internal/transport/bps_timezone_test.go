package transport

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func bpsTimezoneSource(existing bool) map[string]any {
	input := []any{}
	if existing {
		input = append(input, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text",
			"text": "<environment_context>\n  <cwd>/fixture</cwd>\n  <current_date>1999-01-01</current_date>\n  <timezone>UTC</timezone>\n</environment_context>"}}})
	}
	input = append(input, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Keep the original BPS business prompt."}}})
	return map[string]any{"model": "gpt-6-astra", "stream": false, "input": input, "metadata": map[string]any{"fixture_marker": "keep"}}
}

// Filter ordinary input/repair items before using the native date assertion:
// BPS may preserve string content and tool messages in the same history.
func bpsTimezoneArray(value any) []any { values, _ := value.([]any); return values }

func bpsTimezoneEnvironment(t *testing.T, body []byte, zone string) map[string]any {
	t.Helper()
	object, err := protocol.RawObject(body)
	if err != nil {
		t.Fatal(err)
	}
	contexts := []any{}
	for _, value := range bpsTimezoneArray(object["input"]) {
		message := relayObject(value)
		if message["role"] != "user" {
			continue
		}
		texts := []string{}
		if text, ok := message["content"].(string); ok {
			texts = append(texts, text)
		}
		for _, partValue := range bpsTimezoneArray(message["content"]) {
			part := relayObject(partValue)
			if part["type"] == "input_text" {
				if text, ok := part["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		for _, text := range texts {
			if strings.HasPrefix(text, "<environment_context>") {
				contexts = append(contexts, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}})
			}
		}
	}
	nativeTimezoneIntegrationEnvironment(t, protocol.JSONBytes(map[string]any{"input": contexts}), zone)
	if bytes.Contains(body, []byte("internal_chat_message_metadata_passthrough")) || bytes.Contains(body, []byte("__bps_")) {
		t.Error("BPS timezone context exposed an internal/native metadata marker")
	}
	return object
}

func TestBPSTimezoneRewriteAndEndpointMatrix(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		for _, custom := range []bool{false, true} {
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("rewrite=%t/custom=%t/existing=%t", rewrite, custom, existing), func(t *testing.T) {
					tr := New()
					defer tr.Shutdown()
					cfg := protocol.DefaultConfig()
					cfg.NativeTimezoneByIP, cfg.RewriteTools, cfg.TransformResponses = true, rewrite, false
					if custom {
						cfg.ResponsesURL = "https://custom-bps.invalid/v1/responses?fixture=timezone"
					}
					tr.cfg = cfg
					const zone = "Pacific/Kiritimati"
					accessToken := token(t, "timezone-jwt-account")
					var lookups, requests atomic.Int32
					tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
						switch request.URL.String() {
						case nativeTimezoneLookupURL:
							lookups.Add(1)
							nativeTimezoneIntegrationLookup(t, request)
							return nativeTimezoneIntegrationResponse(zone), nil
						case cfg.ResponsesURL:
							requests.Add(1)
							body := nativeTimezoneIntegrationBody(t, request)
							object := bpsTimezoneEnvironment(t, body, zone)
							if !bytes.Contains(body, []byte("Keep the original BPS business prompt.")) || relayObject(object["metadata"])["fixture_marker"] != "keep" {
								t.Error("timezone conversion changed business content or metadata")
							}
							if request.Header.Get("Authorization") != "Bearer "+accessToken || request.Header.Get("ChatGPT-Account-ID") != "selected-parent" {
								t.Error("BPS timezone conversion changed the selected account identity")
							}
							return nativeTimezoneIntegrationResponse(string(protocol.JSONBytes(map[string]any{"output_text": "done"}))), nil
						default:
							t.Error("strict mock rejected an unexpected timezone/BPS target")
							return nil, fmt.Errorf("unexpected fixture target")
						}
					})}
					frames := requestFrames(t, nativeDegradationResponsesURL, accessToken, map[string]string{"ChatGPT-Account-ID": "selected-parent", "Content-Length": "999"}, protocol.JSONBytes(bpsTimezoneSource(existing)))
					result := runForward(t, tr, frames)
					if result.status != http.StatusOK || result.errFrame != nil || !result.ended || lookups.Load() != 1 || requests.Load() != 1 {
						t.Fatalf("BPS timezone matrix failed: result=%+v lookup/request=%d/%d", result, lookups.Load(), requests.Load())
					}
				})
			}
		}
	}
}

func bpsTimezoneBaselineInput(t *testing.T, rewrite bool) []byte {
	t.Helper()
	tr := New()
	defer tr.Shutdown()
	cfg := protocol.DefaultConfig()
	cfg.NativeTimezoneByIP, cfg.RewriteTools, cfg.TransformResponses = false, rewrite, false
	tr.cfg = cfg
	var expectedInput []byte
	tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != cfg.ResponsesURL {
			t.Error("strict baseline mock rejected an unexpected target")
			return nil, fmt.Errorf("unexpected fixture target")
		}
		object, err := protocol.RawObject(nativeTimezoneIntegrationBody(t, request))
		if err != nil {
			t.Fatal(err)
		}
		expectedInput = protocol.JSONBytes(object["input"])
		return nativeTimezoneIntegrationResponse(string(protocol.JSONBytes(map[string]any{"output_text": "done"}))), nil
	})}
	result := runForward(t, tr, requestFrames(t, nativeDegradationResponsesURL, token(t, "acct-7"), nil, protocol.JSONBytes(bpsTimezoneSource(false))))
	if result.status != http.StatusOK || result.errFrame != nil || !result.ended || expectedInput == nil {
		t.Fatalf("BPS baseline request failed: %+v", result)
	}
	return expectedInput
}

func TestBPSTimezoneDisabledAndLookupFailuresPreserveInput(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		// Build an immutable baseline before the subtests so filtered runs do
		// not depend on the disabled sibling having executed first.
		expectedInput := bpsTimezoneBaselineInput(t, rewrite)
		for _, mode := range []string{"disabled", "network", "HTTP", "invalid timezone", "redirect"} {
			t.Run(fmt.Sprintf("rewrite=%t/%s", rewrite, mode), func(t *testing.T) {
				tr := New()
				defer tr.Shutdown()
				cfg := protocol.DefaultConfig()
				cfg.NativeTimezoneByIP, cfg.RewriteTools, cfg.TransformResponses = mode != "disabled", rewrite, false
				tr.cfg = cfg
				source := bpsTimezoneSource(false)
				original := protocol.JSONBytes(source)
				var lookups, requests atomic.Int32
				tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
					switch request.URL.String() {
					case nativeTimezoneLookupURL:
						lookups.Add(1)
						nativeTimezoneIntegrationLookup(t, request)
						response := nativeTimezoneIntegrationResponse("Invalid/Timezone")
						switch mode {
						case "network":
							return nil, fmt.Errorf("synthetic lookup failure")
						case "HTTP":
							response.StatusCode = http.StatusServiceUnavailable
						case "redirect":
							response.StatusCode = http.StatusFound
							response.Header.Set("Location", "https://do-not-follow.invalid/")
						}
						return response, nil
					case cfg.ResponsesURL:
						requests.Add(1)
						body := nativeTimezoneIntegrationBody(t, request)
						if !rewrite && !bytes.Equal(body, original) {
							t.Error("disabled/failed lookup changed original BPS bytes")
						}
						object, err := protocol.RawObject(body)
						if err != nil || !bytes.Equal(protocol.JSONBytes(object["input"]), expectedInput) {
							t.Error("disabled/failed lookup changed BPS input")
						}
						if bytes.Contains(body, []byte("<environment_context>")) {
							t.Error("disabled/failed lookup added a timezone context")
						}
						return nativeTimezoneIntegrationResponse(string(protocol.JSONBytes(map[string]any{"output_text": "done"}))), nil
					default:
						t.Error("strict mock rejected an unexpected target or redirect")
						return nil, fmt.Errorf("unexpected fixture target")
					}
				})}
				for range 2 {
					result := runForward(t, tr, requestFrames(t, nativeDegradationResponsesURL, token(t, "acct-7"), nil, original))
					if result.status != http.StatusOK || result.errFrame != nil || !result.ended {
						t.Fatalf("BPS request did not fail open: %+v", result)
					}
				}
				want := int32(1)
				if mode == "disabled" {
					want = 0
				}
				if requests.Load() != 2 || lookups.Load() != want {
					t.Fatalf("unexpected BPS/lookup count: %d/%d", requests.Load(), lookups.Load())
				}
			})
		}
	}
}

func TestBPSTimezoneSharesNativeCacheUsingResolvedProxyAndClient(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	cfg := protocol.DefaultConfig()
	cfg.NativeTimezoneByIP, cfg.RewriteTools, cfg.TransformResponses = true, false, false
	tr.cfg = cfg
	const actualProxy = "http://actual-user:actual-secret@actual-proxy.invalid:8080"
	const startProxy = "http://wrong-user:wrong-secret@wrong-proxy.invalid:8080"
	const zone = "Asia/Tokyo"
	host := &fakeHost{tokenFor: map[int64]string{7: token(t, "acct-7")}, proxyURLFor: map[int64]string{7: actualProxy}}
	tr.host = host
	tr.client = &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("timezone or business request used the direct/wrong proxy client")
		return nil, fmt.Errorf("unexpected direct fixture request")
	})}
	var lookups, bps, native atomic.Int32
	actualClient := &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case nativeTimezoneLookupURL:
			lookups.Add(1)
			nativeTimezoneIntegrationLookup(t, request)
			return nativeTimezoneIntegrationResponse(zone), nil
		case cfg.ResponsesURL, nativeDegradationResponsesURL:
			body := nativeTimezoneIntegrationBody(t, request)
			if request.URL.String() == cfg.ResponsesURL {
				bps.Add(1)
				bpsTimezoneEnvironment(t, body, zone)
			} else {
				native.Add(1)
				nativeTimezoneIntegrationEnvironment(t, body, zone)
			}
			if request.Header.Get("Authorization") != "Bearer "+host.tokenFor[7] || request.Header.Get("ChatGPT-Account-ID") != "acct-7" {
				t.Error("resolved timezone proxy changed native/BPS identity")
			}
			return nativeTimezoneIntegrationResponse(string(protocol.JSONBytes(map[string]any{"output_text": "done"}))), nil
		default:
			t.Error("strict resolved-proxy mock rejected an unexpected target")
			return nil, fmt.Errorf("unexpected fixture target")
		}
	})}
	tr.proxyClients[actualProxy] = cachedProxyClient{base: tr.client, client: actualClient, lastUsed: time.Now(), order: tr.proxyOrder.PushFront(actualProxy)}
	for _, selectedProxy := range []string{startProxy, startProxy + "/changed"} {
		frames := requestFrames(t, nativeDegradationResponsesURL, "", nil, protocol.JSONBytes(bpsTimezoneSource(false)))
		frames[0].GetStart().ProxyUrl = selectedProxy
		result := runForward(t, tr, frames)
		if result.status != http.StatusOK || result.errFrame != nil || !result.ended {
			t.Fatalf("resolved BPS proxy request failed: %+v", result)
		}
	}
	nativeSource := bpsTimezoneSource(false)
	nativeSource["model"] = "gpt-5.4"
	frames := requestFrames(t, nativeDegradationResponsesURL, host.tokenFor[7], map[string]string{"ChatGPT-Account-ID": "acct-7"}, protocol.JSONBytes(nativeSource))
	frames[0].GetStart().ProxyUrl = actualProxy
	result := runForward(t, tr, frames)
	if result.status != http.StatusOK || result.errFrame != nil || !result.ended {
		t.Fatalf("native shared-cache request failed: %+v", result)
	}
	if lookups.Load() != 1 || bps.Load() != 2 || native.Load() != 1 || len(host.resolvedAccountIDs) != 2 {
		t.Fatalf("resolved proxy/cache identity diverged: lookup=%d BPS=%d native=%d resolve=%d", lookups.Load(), bps.Load(), native.Load(), len(host.resolvedAccountIDs))
	}
	tr.nativeTimezone.mu.Lock()
	defer tr.nativeTimezone.mu.Unlock()
	key := nativeTimezoneKey{accountID: 7, proxyHash: sha256.Sum256([]byte(actualProxy))}
	if len(tr.nativeTimezone.entries) != 1 || tr.nativeTimezone.entries[key] == nil {
		t.Fatal("cache used start proxy instead of actual account egress")
	}
}

func TestBPSTimezoneHTTPRetryReusesPreparedBodyAndSingleLookup(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(fmt.Sprint(rewrite), func(t *testing.T) {
			tr := New()
			defer tr.Shutdown()
			cfg := protocol.DefaultConfig()
			cfg.NativeTimezoneByIP, cfg.RewriteTools, cfg.TransformResponses = true, rewrite, false
			tr.cfg = cfg
			const zone = "Pacific/Kiritimati"
			var lookups, attempts atomic.Int32
			var prepared []byte
			tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
				switch request.URL.String() {
				case nativeTimezoneLookupURL:
					lookups.Add(1)
					nativeTimezoneIntegrationLookup(t, request)
					return nativeTimezoneIntegrationResponse(zone), nil
				case cfg.ResponsesURL:
					body := nativeTimezoneIntegrationBody(t, request)
					bpsTimezoneEnvironment(t, body, zone)
					if attempts.Add(1) == 1 {
						prepared = body
						response := nativeTimezoneIntegrationResponse("synthetic temporary failure")
						response.StatusCode = http.StatusServiceUnavailable
						response.Header.Set("Retry-After", "0")
						return response, nil
					}
					if !bytes.Equal(body, prepared) || request.Header.Get("X-Stainless-Retry-Count") != "1" {
						t.Error("503 retry did not reuse the prepared timezone body")
					}
					return nativeTimezoneIntegrationResponse(string(protocol.JSONBytes(map[string]any{"output_text": "done"}))), nil
				default:
					t.Error("strict retry mock rejected an unexpected target")
					return nil, fmt.Errorf("unexpected fixture target")
				}
			})}
			result := runForward(t, tr, requestFrames(t, nativeDegradationResponsesURL, token(t, "acct-7"), nil, protocol.JSONBytes(bpsTimezoneSource(false))))
			if result.status != http.StatusOK || result.errFrame != nil || !result.ended || attempts.Load() != 2 || lookups.Load() != 1 {
				t.Fatalf("timezone retry failed: result=%+v attempts=%d lookups=%d", result, attempts.Load(), lookups.Load())
			}
		})
	}
}

func TestBPSTimezoneToolRepairPreservesContextPrefixWithoutLookup(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	cfg := protocol.DefaultConfig()
	cfg.NativeTimezoneByIP = true
	tr.cfg = cfg
	source := executorOnlyCatalogSource(t.Name())
	source["stream"] = false
	const zone = "America/New_York"
	var lookups, attempts atomic.Int32
	var initial map[string]any
	tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case nativeTimezoneLookupURL:
			lookups.Add(1)
			nativeTimezoneIntegrationLookup(t, request)
			return nativeTimezoneIntegrationResponse(zone), nil
		case cfg.ResponsesURL:
			body := nativeTimezoneIntegrationBody(t, request)
			object := bpsTimezoneEnvironment(t, body, zone)
			if attempts.Add(1) == 1 {
				initial = object
				wrong := map[string]any{"id": "resp_timezone_visible", "status": "completed", "output": []any{relayNativeCall("call_wrong", "exec_command", map[string]any{"cmd": "pwd"})}}
				return nativeTimezoneIntegrationResponse(string(protocol.JSONBytes(wrong))), nil
			}
			history, corrected := bpsTimezoneArray(initial["input"]), bpsTimezoneArray(object["input"])
			if len(corrected) <= len(history) || !bytes.Equal(protocol.JSONBytes(corrected[:len(history)]), protocol.JSONBytes(history)) {
				t.Error("tool repair replaced or dropped the timezone-bearing history prefix")
			}
			for key, value := range initial {
				if key != "input" && !bytes.Equal(protocol.JSONBytes(value), protocol.JSONBytes(object[key])) {
					t.Errorf("tool repair changed prepared field %s", key)
				}
			}
			fixed := map[string]any{"id": "resp_timezone_internal_repair", "status": "completed", "output": []any{relayNativeCall("call_repaired", "functions.exec", "text(await tools.exec_command({cmd: 'pwd'}));")}}
			return nativeTimezoneIntegrationResponse(string(protocol.JSONBytes(fixed))), nil
		default:
			t.Error("strict repair mock rejected an unexpected target")
			return nil, fmt.Errorf("unexpected fixture target")
		}
	})}
	result := runForward(t, tr, requestFrames(t, nativeDegradationResponsesURL, token(t, "acct-7"), nil, protocol.JSONBytes(source)))
	if result.status != http.StatusOK || result.errFrame != nil || !result.ended || attempts.Load() != 2 || lookups.Load() != 1 ||
		!bytes.Contains(result.body, []byte("call_repaired")) || !bytes.Contains(result.body, []byte("custom_tool_call")) {
		t.Fatalf("timezone tool repair failed: result=%+v attempts=%d lookups=%d", result, attempts.Load(), lookups.Load())
	}
}
