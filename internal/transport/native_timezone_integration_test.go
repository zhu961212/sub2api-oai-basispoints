package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func nativeTimezoneIntegrationBody(t *testing.T, request *http.Request) []byte {
	t.Helper()
	if request.Body == nil || request.GetBody == nil {
		t.Fatal("forwarded body cannot be reread")
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = request.Body.Close()
	copyBody, err := request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := io.ReadAll(copyBody)
	_ = copyBody.Close()
	if err != nil || !bytes.Equal(body, duplicate) || request.ContentLength != int64(len(body)) {
		t.Fatalf("request body, GetBody and ContentLength disagree: %d/%d/%d; %v", len(body), len(duplicate), request.ContentLength, err)
	}
	if request.Header.Get("Content-Length") != "" {
		t.Error("stale Content-Length header survived request creation")
	}
	return body
}

func nativeTimezoneIntegrationEnvironment(t *testing.T, body []byte, zone string) {
	t.Helper()
	var object struct {
		Input []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, message := range object.Input {
		for _, part := range message.Content {
			if message.Role != "user" || part.Type != "input_text" || !strings.HasPrefix(part.Text, "<environment_context>") {
				continue
			}
			found++
			now := time.Now()
			yesterdayEdge := now.Add(-time.Second).In(location).Format("2006-01-02")
			tomorrowEdge := now.Add(time.Second).In(location).Format("2006-01-02")
			if !strings.Contains(part.Text, "<timezone>"+zone+"</timezone>") ||
				(!strings.Contains(part.Text, "<current_date>"+yesterdayEdge+"</current_date>") && !strings.Contains(part.Text, "<current_date>"+tomorrowEdge+"</current_date>")) {
				t.Errorf("native environment has wrong timezone or current local date: %q", part.Text)
			}
			if strings.Contains(part.Text, "1999-01-01") {
				t.Error("old current date survived timezone update")
			}
		}
	}
	if found != 1 {
		t.Fatalf("expected one native environment context, got %d", found)
	}
}

func nativeTimezoneIntegrationLookup(t *testing.T, request *http.Request) {
	t.Helper()
	if request.Method != http.MethodGet || request.URL.String() != "https://ipapi.co/timezone/" || request.Body != nil {
		t.Errorf("unexpected timezone lookup request: %s %s", request.Method, request.URL)
	}
	if len(request.Header) != 0 || request.URL.User != nil {
		t.Error("lookup leaked account credentials, headers or cookies")
	}
	deadline, ok := request.Context().Deadline()
	if !ok || time.Until(deadline) > 2*time.Second {
		t.Error("lookup has no bounded independent deadline")
	}
}

func nativeTimezoneIntegrationResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestNativeTimezoneIntegrationForwardAndProbeShareAccountProxyCache(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	cfg := protocol.DefaultConfig()
	cfg.NativeTimezoneByIP = true
	tr.cfg = cfg
	const zone = "Pacific/Kiritimati"
	const proxyA = "http://fixture-a:private-a@proxy-a.invalid:8080"
	const proxyB = "http://fixture-b:private-b@proxy-b.invalid:8080"
	host := &fakeHost{tokenFor: map[int64]string{7: token(t, "acct-7"), 9: token(t, "acct-9")}, proxyURLFor: map[int64]string{7: proxyA, 9: proxyB}}
	jar, _ := cookiejar.New(nil)
	lookupURL, _ := url.Parse("https://ipapi.co/timezone/")
	jar.SetCookies(lookupURL, []*http.Cookie{{Name: "private-session", Value: "do-not-share"}})
	var lookups, nativeRequests atomic.Int32
	tr.client = &http.Client{Jar: jar, Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case "https://ipapi.co/timezone/":
			lookups.Add(1)
			nativeTimezoneIntegrationLookup(t, request)
			return nativeTimezoneIntegrationResponse(zone), nil
		case nativeDegradationResponsesURL, nativeDegradationResponsesURL + "/compact":
			nativeRequests.Add(1)
			if request.Method != http.MethodPost {
				t.Error("native request method changed")
			}
			account := request.Header.Get("ChatGPT-Account-ID")
			expected := host.tokenFor[7]
			if account == "acct-9" {
				expected = host.tokenFor[9]
			} else if account != "acct-7" {
				t.Errorf("unexpected account identity %q", account)
			}
			if request.Header.Get("Authorization") != "Bearer "+expected {
				t.Error("native account authorization changed")
			}
			body := nativeTimezoneIntegrationBody(t, request)
			nativeTimezoneIntegrationEnvironment(t, body, zone)
			return nativeTimezoneIntegrationResponse(`{"output_text":"iPhone 17"}`), nil
		default:
			t.Errorf("strict mock rejected unexpected target: %s", request.URL)
			return nil, fmt.Errorf("unexpected target")
		}
	})}
	original := protocol.JSONBytes(map[string]any{"model": "gpt-5.4", "input": []map[string]any{
		{"role": "user", "content": []map[string]string{{"type": "input_text", "text": "<environment_context>\n  <cwd>/fixture</cwd>\n  <current_date>1999-01-01</current_date>\n  <timezone>UTC</timezone>\n</environment_context>"}}},
		{"role": "user", "content": []map[string]string{{"type": "input_text", "text": "Keep the original business prompt."}}},
	}})
	forward := func(accountID int64, proxy, endpoint string) {
		frames := requestFrames(t, endpoint, host.tokenFor[accountID], map[string]string{"Content-Type": "application/json", "ChatGPT-Account-ID": fmt.Sprintf("acct-%d", accountID), "Content-Length": "999"}, original)
		frames[0].GetStart().AccountId, frames[0].GetStart().ProxyUrl = accountID, proxy
		result := runForward(t, tr, frames)
		if result.status != http.StatusOK || result.errFrame != nil || !result.ended {
			t.Fatalf("native forwarding failed: %+v", result)
		}
	}
	probe := func(accountID int64) {
		status, answer, err := tr.checkDegradationAccount(context.Background(), cfg, host, tr.client, accountID, cfg.DegradationCheckModel)
		if err != nil || status != "ok" || answer != "iPhone 17" {
			t.Fatalf("native probe failed: %s %q %v", status, answer, err)
		}
	}
	forward(7, proxyA, nativeDegradationResponsesURL)
	probe(7)
	forward(7, proxyA, nativeDegradationResponsesURL+"/compact")
	if lookups.Load() != 1 {
		t.Fatalf("native requests with the same account and proxy made %d lookups", lookups.Load())
	}
	host.proxyURLFor[7] = proxyB
	forward(7, proxyB, nativeDegradationResponsesURL)
	probe(7)
	if lookups.Load() != 2 {
		t.Fatalf("proxy change did not isolate cache: lookups=%d", lookups.Load())
	}
	forward(9, proxyB, nativeDegradationResponsesURL)
	probe(9)
	if lookups.Load() != 3 || nativeRequests.Load() != 7 {
		t.Fatalf("account cache isolation or dispatch count wrong: %d/%d", lookups.Load(), nativeRequests.Load())
	}
}

func TestNativeTimezoneIntegrationForwardSkipsUnmatchedRequests(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, body, authority, encoding, method string
		enabled, bps                                      bool
		accountID                                         int64
	}{
		{name: "disabled", endpoint: nativeDegradationResponsesURL, body: `{"model":"gpt-5.4","input":"hello"}`, accountID: 7},
		{name: "invalid JSON", endpoint: nativeDegradationResponsesURL, body: `{"model":`, enabled: true, accountID: 7},
		{name: "custom endpoint", endpoint: "https://gateway.invalid/backend-api/codex/responses", body: `{"model":"gpt-5.4","input":"hello"}`, enabled: true, accountID: 7},
		{name: "custom authority", endpoint: nativeDegradationResponsesURL, authority: "gateway.invalid", body: `{"model":"gpt-5.4","input":"hello"}`, enabled: true, accountID: 7},
		{name: "disabled BPS route", endpoint: nativeDegradationResponsesURL, body: `{"model":"gpt-6-astra","input":"hello"}`, bps: true, accountID: 7},
		{name: "missing account", endpoint: nativeDegradationResponsesURL, body: `{"model":"gpt-5.4","input":"hello"}`, enabled: true},
		{name: "encoded body", endpoint: nativeDegradationResponsesURL, body: `{"model":"gpt-5.4","input":"hello"}`, encoding: "gzip", enabled: true, accountID: 7},
		{name: "non POST", endpoint: nativeDegradationResponsesURL, body: `{"model":"gpt-5.4","input":"hello"}`, method: http.MethodGet, enabled: true, accountID: 7},
		{name: "other native path", endpoint: "https://chatgpt.com/backend-api/other", body: `{"model":"gpt-5.4","input":"hello"}`, enabled: true, accountID: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := New()
			defer tr.Shutdown()
			cfg := protocol.DefaultConfig()
			cfg.NativeTimezoneByIP, cfg.RewriteTools, cfg.TransformResponses = tc.enabled, false, false
			tr.cfg = cfg
			expectedURL := tc.endpoint
			if tc.bps {
				expectedURL = cfg.ResponsesURL
			}
			var requests int
			tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
				requests++
				if request.URL.String() != expectedURL {
					t.Errorf("strict mock rejected unexpected target: %s", request.URL)
					return nil, fmt.Errorf("unexpected target")
				}
				if body := nativeTimezoneIntegrationBody(t, request); string(body) != tc.body {
					t.Errorf("skipped request body changed: %q", body)
				}
				if !tc.bps && (request.Header.Get("X-Test-Original") != "unchanged" || request.Header.Get("Authorization") != "Bearer fixture-native-token") {
					t.Error("passthrough headers changed")
				}
				return nativeTimezoneIntegrationResponse(`{"output_text":"iPhone 17"}`), nil
			})}
			frames := requestFrames(t, tc.endpoint, "fixture-native-token", map[string]string{"ChatGPT-Account-ID": "fixture-native", "X-Test-Original": "unchanged", "Content-Encoding": tc.encoding}, []byte(tc.body))
			frames[0].GetStart().AccountId, frames[0].GetStart().Host = tc.accountID, tc.authority
			if tc.method != "" {
				frames[0].GetStart().Method = tc.method
			}
			result := runForward(t, tr, frames)
			if result.status != http.StatusOK || result.errFrame != nil || !result.ended || requests != 1 {
				t.Fatalf("skip path failed or queried metadata: %+v; requests=%d", result, requests)
			}
		})
	}
}

func TestNativeTimezoneIntegrationLookupFailuresPreserveNativeBusinessAndProbe(t *testing.T) {
	for _, mode := range []string{"HTTP failure", "redirect", "invalid zone", "network failure"} {
		t.Run(mode, func(t *testing.T) {
			tr := New()
			defer tr.Shutdown()
			cfg := protocol.DefaultConfig()
			cfg.NativeTimezoneByIP = true
			tr.cfg = cfg
			host := &fakeHost{token: token(t, "fixture-native")}
			expected, _, err := prepareNativeDegradationRequest(context.Background(), host, 7, cfg.DegradationCheckModel)
			if err != nil {
				t.Fatal(err)
			}
			expectedProbe, err := io.ReadAll(expected.Body)
			_ = expected.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			original := `{"model":"gpt-5.4","input":"original text stays byte-for-byte unchanged"}`
			jar, _ := cookiejar.New(nil)
			lookupURL, _ := url.Parse("https://ipapi.co/timezone/")
			jar.SetCookies(lookupURL, []*http.Cookie{{Name: "private-session", Value: "do-not-share"}})
			var lookups, nativeRequests, redirects atomic.Int32
			tr.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { redirects.Add(1); return nil }, Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
				switch request.URL.String() {
				case "https://ipapi.co/timezone/":
					lookups.Add(1)
					nativeTimezoneIntegrationLookup(t, request)
					response := nativeTimezoneIntegrationResponse("Invalid/Timezone")
					switch mode {
					case "HTTP failure":
						response.StatusCode = http.StatusServiceUnavailable
					case "redirect":
						response.StatusCode = http.StatusFound
						response.Header.Set("Location", "https://must-not-follow.invalid/timezone/")
					case "network failure":
						return nil, fmt.Errorf("fixture lookup unavailable")
					}
					return response, nil
				case nativeDegradationResponsesURL:
					index := nativeRequests.Add(1)
					body := nativeTimezoneIntegrationBody(t, request)
					if request.Header.Get("Authorization") != "Bearer "+host.token || request.Header.Get("ChatGPT-Account-ID") != "fixture-native" {
						t.Error("fail-open changed native account authorization")
					}
					if index == 1 {
						if string(body) != original || request.Header.Get("Cookie") != "native-cookie=keep" || request.Header.Get("X-Test-Original") != "keep" {
							t.Error("lookup failure changed business body or headers")
						}
					} else if index == 2 {
						if !bytes.Equal(body, expectedProbe) {
							t.Error("lookup failure changed native probe body")
						}
						if request.Header.Get("Cookie") != "" {
							t.Error("lookup cookies leaked into native probe")
						}
					} else {
						t.Error("native request was retried")
					}
					return nativeTimezoneIntegrationResponse(`{"output_text":"iPhone 17"}`), nil
				default:
					t.Errorf("strict mock rejected unexpected target: %s", request.URL)
					return nil, fmt.Errorf("unexpected target")
				}
			})}
			frames := requestFrames(t, nativeDegradationResponsesURL, host.token, map[string]string{"ChatGPT-Account-ID": "fixture-native", "Cookie": "native-cookie=keep", "X-Test-Original": "keep"}, []byte(original))
			result := runForward(t, tr, frames)
			if result.status != http.StatusOK || result.errFrame != nil || !result.ended {
				t.Fatalf("business request did not fail open: %+v", result)
			}
			status, answer, err := tr.checkDegradationAccount(context.Background(), cfg, host, tr.client, 7, cfg.DegradationCheckModel)
			if err != nil || status != "ok" || answer != "iPhone 17" {
				t.Fatalf("probe did not fail open: %s %q %v", status, answer, err)
			}
			if lookups.Load() != 1 || nativeRequests.Load() != 2 || redirects.Load() != 0 {
				t.Fatalf("failed lookup cache or redirect isolation incorrect: %d/%d/%d", lookups.Load(), nativeRequests.Load(), redirects.Load())
			}
		})
	}
}
