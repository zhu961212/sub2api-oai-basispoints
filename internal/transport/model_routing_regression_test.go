package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestUnsupportedModelsPreserveHostPassthrough(t *testing.T) {
	cases := []struct{ name, body string }{}
	for _, model := range []string{
		"gpt-5.4", "gpt-5-codex", "gpt-5.6-luna", "gpt-5.6-terra",
		"gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol-excel", "gpt-5.6-luna-excel",
		"gpt-6-astra-excel", "gpt-6-astra-preview", "GPT-6-ASTRA", "gpt-6-Astra",
		"gpt-5.6-sol-preview", "GPT-5.6-SOL", "gpt-5.6-Sol",
		"custom-alias", "future-model", "", "   ",
	} {
		cases = append(cases, struct{ name, body string }{model, `{ "model" : ` + string(protocol.JSONBytes(model)) + `, "input":"preserve this request", "tools":[{"type":"function","name":"demo"}], "extra":9007199254740993 }`})
	}
	for name, body := range map[string]string{
		"uppercase key":             `{"MODEL":"gpt-6-astra"}`,
		"conflicting uppercase key": `{"model":"gpt-6-sol","MODEL":"gpt-6-astra"}`,
		"uppercase sol key":         `{"MODEL":"gpt-5.6-sol"}`,
		"conflicting sol key":       `{"model":"gpt-6-sol","MODEL":"gpt-5.6-sol"}`,
		"nested model":              `{"metadata":{"model":"gpt-6-astra"}}`,
		"nested sol model":          `{"metadata":{"model":"gpt-5.6-sol"}}`,
		"missing model":             `{}`,
		"null model":                `{"model":null}`,
		"numeric model":             `{"model":42}`,
		"array body":                `[{"model":"gpt-6-astra"}]`,
		"malformed body":            `{"model":"gpt-6-astra"`,
		"trailing object":           `{"model":"gpt-6-astra"}{}`,
	} {
		cases = append(cases, struct{ name, body string }{name, body})
	}
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			type capturedRequest struct {
				method, uri string
				headers     http.Header
				body        []byte
			}
			captured := make(chan capturedRequest, 1)
			responseBody := []byte(`{ "id":"resp_host", "output":[{"type":"function_call","name":"run_officejs","arguments":"{}"}], "extra":9007199254740993 }`)
			hostUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				captured <- capturedRequest{r.Method, r.RequestURI, r.Header.Clone(), body}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Add("X-Host-Trace", "first")
				w.Header().Add("X-Host-Trace", "second")
				w.WriteHeader(status)
				_, _ = w.Write(responseBody)
			}))
			defer hostUpstream.Close()
			var basisHits atomic.Int32
			basisPoints := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				basisHits.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer basisPoints.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{
				"responses_url": basisPoints.URL,
			})
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					const path = "/v1/responses?api-version=custom&value=%2F"
					frames := requestFrames(t, hostUpstream.URL+path, "host-supplied-token", map[string]string{
						"Openai-Beta": "responses=v1", "Accept": "application/octet-stream",
						"ChatGPT-Account-ID": "host-account",
					}, []byte(test.body))
					frames[0].GetStart().Method = http.MethodPatch
					frames[0].GetStart().Headers["X-Client-Trace"] = &pluginv1.HeaderValues{Values: []string{"one", "two"}}
					result := runForward(t, transport, frames)
					if result.errFrame != nil || result.status != status || !result.ended || !bytes.Equal(result.body, responseBody) {
						t.Fatalf("host response changed: %#v, body %s", result, result.body)
					}
					if basisHits.Load() != 0 {
						t.Fatal("unsupported request reached Basis Points")
					}
					select {
					case request := <-captured:
						if request.method != http.MethodPatch || request.uri != path || string(request.body) != test.body {
							t.Fatalf("host request changed: %#v", request)
						}
						for key, value := range map[string]string{"Authorization": "Bearer host-supplied-token", "Openai-Beta": "responses=v1", "Accept": "application/octet-stream", "ChatGPT-Account-ID": "host-account"} {
							if request.headers.Get(key) != value {
								t.Errorf("header %s changed: %q", key, request.headers.Get(key))
							}
						}
						if !reflect.DeepEqual(request.headers.Values("X-Client-Trace"), []string{"one", "two"}) || request.headers.Get("X-Basispoints-Auth-Mode") != "" {
							t.Fatalf("request headers changed: %#v", request.headers)
						}
					default:
						t.Fatal("host upstream was not contacted")
					}
					if got := result.headers["X-Host-Trace"].GetValues(); !reflect.DeepEqual(got, []string{"first", "second"}) {
						t.Fatalf("host response headers changed: %#v", result.headers)
					}
				})
			}
		})
	}
}

func TestChannelMonitorChallengeUsesSelectedModelRouting(t *testing.T) {
	const marker = "Calculate and respond with ONLY the number, nothing else."
	body := []byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"` + marker + `\n\nQ: 3 + 5 = ?\nA:"}],"stream":false}`)
	var hostHits, basisHits atomic.Int32
	var gotBody []byte
	hostUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"monitor","choices":[{"message":{"content":"8"}}]}`))
	}))
	defer hostUpstream.Close()
	basisPoints := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		basisHits.Add(1)
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(relayTestCompletedResponse())
	}))
	defer basisPoints.Close()

	transport := New()
	defer transport.Shutdown()
	applyConfig(t, transport, map[string]any{"responses_url": basisPoints.URL, "enabled_models": []string{"gpt-6-astra"}, "rewrite_tools": false, "transform_responses": false})
	result := runForward(t, transport, requestFrames(t, hostUpstream.URL+"/v1/chat/completions", token(t, "monitor-account"), nil, body))
	if result.errFrame != nil || result.status != http.StatusOK || !result.ended {
		t.Fatalf("challenge request did not follow its selected model: %#v", result)
	}
	if hostHits.Load() != 0 || basisHits.Load() != 1 {
		t.Fatalf("challenge routing host=%d basis=%d, want host=0 basis=1", hostHits.Load(), basisHits.Load())
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("probe body changed: got %s want %s", gotBody, body)
	}
}

func TestSupportedModelRoutingKeepsWhitelistAndRequestedUpstream(t *testing.T) {
	type routingCase struct {
		name, model string
		account     int64
		whitelist   []int64
		wantBasis   bool
		enabled     []string
	}
	cases := []routingCase{
		{"exact astra", "gpt-6-astra", 7, nil, true, nil},
		{"trimmed astra", " gpt-6-astra ", 7, nil, true, nil},
		{"astra listed account", "gpt-6-astra", 7, []int64{7}, true, nil},
		{"astra unlisted account", "gpt-6-astra", 7, []int64{42}, false, nil},
		{"astra unknown account keeps existing allowance", "gpt-6-astra", 0, []int64{42}, true, nil},
		{"exact sol", "gpt-5.6-sol", 7, nil, true, nil},
		{"trimmed sol", " \tgpt-5.6-sol\n", 7, nil, true, nil},
		{"sol listed account", "gpt-5.6-sol", 7, []int64{7}, true, nil},
		{"sol unlisted account", "gpt-5.6-sol", 7, []int64{42}, false, nil},
		{"sol unknown account keeps existing allowance", "gpt-5.6-sol", 0, []int64{42}, true, nil},
	}
	for _, model := range protocol.AvailableModels() {
		others := make([]string, 0)
		for _, other := range protocol.AvailableModels() {
			if model != other {
				others = append(others, other)
			}
		}
		cases = append(cases,
			routingCase{model + " enabled", model, 7, nil, true, []string{model}},
			routingCase{model + " all enabled", model, 7, nil, true, protocol.AvailableModels()},
			routingCase{model + " trimmed", " \t" + model + "\n", 7, nil, true, []string{model}},
			routingCase{model + " listed account", model, 7, []int64{7}, true, []string{model}},
			routingCase{model + " unlisted account", model, 7, []int64{42}, false, []string{model}},
			routingCase{model + " unknown account keeps allowance", model, 0, []int64{42}, true, []string{model}},
			routingCase{model + " unselected", model, 7, nil, false, others},
			routingCase{model + " none selected", model, 7, nil, false, []string{}},
			routingCase{model + " uppercase passthrough", strings.ToUpper(model), 7, nil, false, protocol.AvailableModels()},
			routingCase{model + " legacy alias passthrough", model + "-excel", 7, nil, false, protocol.AvailableModels()},
		)
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var hostHits, basisHits atomic.Int32
			basisModels := make(chan string, 1)
			hostBodies := make(chan []byte, 1)
			responseBody := []byte(`{"id":"resp_routing","status":"completed","output":[]}`)
			hostUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hostHits.Add(1)
				body, _ := io.ReadAll(r.Body)
				hostBodies <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(responseBody)
			}))
			defer hostUpstream.Close()
			basisPoints := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				basisHits.Add(1)
				body, _ := io.ReadAll(r.Body)
				basisModels <- protocol.RequestedModel(body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(responseBody)
			}))
			defer basisPoints.Close()
			transport := New()
			defer transport.Shutdown()
			applyConfig(t, transport, map[string]any{
				"responses_url": basisPoints.URL, "account_ids": test.whitelist,
				"enabled_models": test.enabled,
			})
			body := protocol.JSONBytes(map[string]any{"model": test.model, "input": "hi"})
			frames := requestFrames(t, hostUpstream.URL, token(t, "scheduled-account"), nil, body)
			frames[0].GetStart().AccountId = test.account
			result := runForward(t, transport, frames)
			if result.errFrame != nil || result.status != http.StatusOK || !result.ended {
				t.Fatalf("routing failed: %#v", result)
			}
			if test.wantBasis {
				if basisHits.Load() != 1 || hostHits.Load() != 0 {
					t.Fatal("supported request did not exclusively reach Basis Points")
				}
				if model, want := <-basisModels, strings.TrimSpace(test.model); model != want {
					t.Fatalf("upstream model changed to %q, want %q", model, want)
				}
			} else if basisHits.Load() != 0 || hostHits.Load() != 1 {
				t.Fatal("whitelist did not keep the request on the host upstream")
			} else if got := <-hostBodies; !bytes.Equal(got, body) {
				t.Fatal("passthrough request body changed")
			}
		})
	}
}
