package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Keep the original clone as an independent normalization and performance
// reference. In particular, marshal errors continue to produce nil.
func cloneJSONValueViaEncoding(value any) any {
	raw, _ := json.Marshal(value)
	var copy any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&copy)
	return copy
}

type cloneTestMarshaler string

func (m cloneTestMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(m), nil
}

func TestCloneJSONValueMatchesNormalization(t *testing.T) {
	var deep any = "leaf"
	for range 80 {
		deep = []any{deep}
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	shared := map[string]any{"values": []any{"shared", json.Number("9007199254740993")}}
	cases := map[string]any{
		"canonical":           map[string]any{"nested": []any{true, false, nil, json.Number("9007199254740993"), json.Number("-1.2e+50"), "line\n<>&\u2028\u2029中文😀"}},
		"control and unicode": string([]rune{0, 1, 8, 9, 10, 12, 13, 0x2028, 0x2029, 0x1f600}),
		"number grammar":      []any{json.Number("-0"), json.Number("1e+99"), json.Number("0.123")},
		"empty containers":    map[string]any{"map": map[string]any{}, "list": []any{}},
		"nil containers":      map[string]any{"map": map[string]any(nil), "list": []any(nil)},
		"empty number":        json.Number(""),
		"invalid number":      json.Number("01"),
		"spaced number":       json.Number("1 "),
		"non number":          json.Number("null"),
		"invalid utf8 key":    map[string]any{string([]byte{0xff}): "value"},
		"invalid utf8 value":  map[string]any{"key": string([]byte{0xfe})},
		"typed containers":    map[string]any{"list": []string{"one", "two"}},
		"numbers":             []any{int64(9007199254740993), uint64(math.MaxUint64), 1.23e-7, float32(1.25)},
		"nonfinite":           math.Inf(1),
		"custom marshaler":    cloneTestMarshaler(jsonBytes(shared)),
		"invalid marshaler":   cloneTestMarshaler("not JSON"),
		"byte slice":          []byte{0, 1, 255},
		"deep":                deep,
		"cycle":               cycle,
		"shared subtree":      []any{shared, shared},
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			want := cloneJSONValueViaEncoding(value)
			if got := cloneJSONValue(value); !reflect.DeepEqual(got, want) {
				t.Fatalf("clone differs from encoding/json: got %#v, want %#v", got, want)
			}
		})
	}
}

func TestCloneJSONValuePreservesRequestsLargerThanCacheBudget(t *testing.T) {
	history := strings.Repeat("x", protocolCacheEntryBytes+1)
	source := map[string]any{"input": []any{messageItem("user", history)}}
	copy := cloneJSONValue(source)
	if !reflect.DeepEqual(copy, source) {
		t.Fatal("request-local cloning truncated history beyond the retained cache budget")
	}
}

func TestCloneJSONValueSeparatesSharedSubtreesAndCallers(t *testing.T) {
	shared := map[string]any{"values": []any{"original"}}
	source := []any{shared, shared}
	const callers = 32
	var workers sync.WaitGroup
	errors := make(chan string, callers)
	for caller := range callers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			copy := cloneJSONValue(source).([]any)
			first := copy[0].(map[string]any)
			first["values"].([]any)[0] = fmt.Sprintf("caller_%d", caller)
			first["added"] = true
			second := copy[1].(map[string]any)
			if second["values"].([]any)[0] != "original" || second["added"] != nil {
				errors <- "cloned subtrees share mutable containers"
			}
		}()
	}
	workers.Wait()
	close(errors)
	for message := range errors {
		t.Error(message)
	}
	if shared["values"].([]any)[0] != "original" || shared["added"] != nil {
		t.Fatal("cloned callers modified the source")
	}
}

func FuzzCloneJSONValueMatchesNormalization(f *testing.F) {
	f.Add(jsonBytes(map[string]any{"number": json.Number("9007199254740993"), "string": "<>&中文", "nested": []any{nil, true, false}}))
	f.Add([]byte("[{},[],null]"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil {
			t.Skip()
		}
		if got, want := cloneJSONValue(value), cloneJSONValueViaEncoding(value); !reflect.DeepEqual(got, want) {
			t.Fatalf("clone differs from encoding/json: got %#v, want %#v", got, want)
		}
	})
}

func BenchmarkProtocolJSONClone(b *testing.B) {
	history := make([]any, 64)
	for index := range history {
		history[index] = messageItem("user", strings.Repeat("Keep complete request history. ", 64))
	}
	for name, value := range map[string]any{"Catalog": cacheBenchmarkCatalog(), "History": history} {
		b.Run(name, func(b *testing.B) {
			for _, test := range []struct {
				name  string
				clone func(any) any
			}{{"JSONRoundTrip", cloneJSONValueViaEncoding}, {"CanonicalCopy", cloneJSONValue}} {
				b.Run(test.name, func(b *testing.B) {
					for _, parallel := range []bool{false, true} {
						b.Run(fmt.Sprintf("parallel=%t", parallel), func(b *testing.B) {
							b.ReportAllocs()
							if parallel {
								b.RunParallel(func(pb *testing.PB) {
									for pb.Next() {
										if test.clone(value) == nil {
											b.Error("valid benchmark data rejected")
										}
									}
								})
							} else {
								for range b.N {
									if test.clone(value) == nil {
										b.Fatal("valid benchmark data rejected")
									}
								}
							}
						})
					}
				})
			}
		})
	}
}
