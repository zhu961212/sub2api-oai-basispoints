package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestNonAstraModelsPreserveHostPassthrough(t *testing.T) {
	cases := []struct{ name, body string }{}
	for _, model := range []string{
		"gpt-5.4", "gpt-5-codex", "gpt-5.6-sol", "gpt-5.6-luna", "gpt-5.6-terra",
		"gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol-excel", "gpt-5.6-luna-excel",
		"gpt-6-astra-excel", "gpt-6-astra-preview", "GPT-6-ASTRA", "gpt-6-Astra",
		"custom-alias", "future-model", "", "   ",
	} {
		cases = append(cases, struct{ name, body string }{model, `{ "model" : ` + string(protocol.JSONBytes(model)) + `, "input":"preserve this request", "tools":[{"type":"function","name":"demo"}], "extra":9007199254740993 }`})
	}
	for name, body := range map[string]string{
		"uppercase key":             `{"MODEL":"gpt-6-astra"}`,
		"conflicting uppercase key": `{"model":"gpt-6-sol","MODEL":"gpt-6-astra"}`,
		"nested model":              `{"metadata":{"model":"gpt-6-astra"}}`,
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
				"responses_url":  basisPoints.URL,
				"models":         []string{"gpt-6-sol", "custom-alias", "GPT-6-ASTRA"},
				"model_map":      map[string]string{"gpt-6-sol": "gpt-6-astra", "custom-alias": "gpt-6-astra"},
				"upstream_model": "forced-model",
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
						t.Fatal("non-Astra request reached Basis Points")
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

func TestAstraRoutingKeepsWhitelistAndFixedUpstream(t *testing.T) {
	for _, test := range []struct {
		name, model string
		account     int64
		whitelist   []int64
		wantBasis   bool
	}{
		{"exact model", "gpt-6-astra", 7, nil, true},
		{"trimmed model", " gpt-6-astra ", 7, nil, true},
		{"listed account", "gpt-6-astra", 7, []int64{7}, true},
		{"unlisted account", "gpt-6-astra", 7, []int64{42}, false},
		{"unknown account keeps existing allowance", "gpt-6-astra", 0, []int64{42}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var hostHits, basisHits atomic.Int32
			basisModels := make(chan string, 1)
			responseBody := []byte(`{"id":"resp_routing","status":"completed","output":[]}`)
			hostUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hostHits.Add(1)
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
				"upstream_model": "forced-model", "models": []string{"custom-alias"},
				"model_map": map[string]string{"gpt-6-astra": "forced-model"},
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
					t.Fatal("Astra request did not exclusively reach Basis Points")
				}
				if model := <-basisModels; model != "gpt-6-astra" {
					t.Fatalf("upstream model changed to %q", model)
				}
			} else if basisHits.Load() != 0 || hostHits.Load() != 1 {
				t.Fatal("whitelist did not keep the request on the host upstream")
			}
		})
	}
}
