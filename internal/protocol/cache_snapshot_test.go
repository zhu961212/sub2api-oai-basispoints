package protocol

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

// Keep the previous admission path as an independent semantic and benchmark
// reference. Canonical snapshots must preserve encoding/json normalization.
func cacheSnapshotViaJSON(value any) (any, int, bool) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > protocolCacheEntryBytes {
		return nil, 0, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var owned any
	if decoder.Decode(&owned) != nil {
		return nil, 0, false
	}
	return owned, protocolCacheWeight(owned), true
}

func TestProtocolCacheSnapshotPreservesJSONSemantics(t *testing.T) {
	var deep any = "leaf"
	for range 80 {
		deep = []any{deep}
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	cases := map[string]any{
		"canonical":        map[string]any{"nested": []any{true, false, nil, json.Number("9007199254740993"), json.Number("-1.2e+50"), "line\n<>&\u2028\u2029中文"}},
		"empty containers": map[string]any{"map": map[string]any{}, "list": []any{}},
		"nil containers":   map[string]any{"map": map[string]any(nil), "list": []any(nil)},
		"empty number":     json.Number(""),
		"invalid number":   json.Number("01"),
		"spaced number":    json.Number("1 "),
		"non number":       json.Number("null"),
		"invalid utf8":     map[string]any{string([]byte{0xff}): string([]byte{0xfe})},
		"typed containers": map[string]any{"list": []string{"one", "two"}},
		"numbers":          []any{int64(9007199254740993), uint64(math.MaxUint64), 1.23e-7, float32(1.25)},
		"nonfinite":        math.Inf(1),
		"custom marshaler": json.RawMessage("{\"value\":9007199254740993}"),
		"byte slice":       []byte{0, 1, 255},
		"deep":             deep,
		"cycle":            cycle,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			want, wantWeight, wantOK := cacheSnapshotViaJSON(input)
			got, gotWeight, gotOK := protocolCacheSnapshot(input)
			if gotOK != wantOK || gotWeight != wantWeight || !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot differs from encoding/json: got (%#v, %d, %t), want (%#v, %d, %t)", got, gotWeight, gotOK, want, wantWeight, wantOK)
			}
		})
	}
}

func TestProtocolCacheSnapshotOwnsContainersAndStringStorage(t *testing.T) {
	backing := strings.Repeat("1234567890", 1<<17)
	key, value, number := backing[10:13], backing[20:28], json.Number(backing[30:45])
	source := map[string]any{key: []any{map[string]any{"value": value, "number": number}}}
	owned, _, ok := protocolCacheSnapshot(source)
	if !ok {
		t.Fatal("valid snapshot rejected")
	}
	copy := owned.(map[string]any)
	for ownedKey := range copy {
		if unsafe.StringData(ownedKey) == unsafe.StringData(key) {
			t.Fatal("cache key retains oversized caller backing storage")
		}
	}
	nested := copy[key].([]any)[0].(map[string]any)
	if unsafe.StringData(nested["value"].(string)) == unsafe.StringData(value) || unsafe.StringData(string(nested["number"].(json.Number))) == unsafe.StringData(string(number)) {
		t.Fatal("cache value retains oversized caller backing storage")
	}
	source[key].([]any)[0].(map[string]any)["value"] = "changed"
	source[key].([]any)[0] = nil
	delete(source, key)
	if nested["value"] != value || nested["number"] != number {
		t.Fatal("caller mutation changed snapshot")
	}
}

func TestProtocolCacheSnapshotRejectsEncodedAndRetainedOversize(t *testing.T) {
	// HTML characters expand sixfold in json.Marshal's default encoding.
	accepted := strings.Repeat("<", (protocolCacheEntryBytes-2)/6)
	if _, _, ok := protocolCacheSnapshot(accepted); !ok {
		t.Fatal("valid near-boundary encoded string rejected")
	}
	for name, value := range map[string]any{
		"encoded":  accepted + "<",
		"string":   strings.Repeat("x", protocolCacheEntryBytes),
		"retained": make([]any, protocolCacheEntryBytes/32+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, ok := protocolCacheSnapshot(value); ok {
				t.Fatal("oversized snapshot admitted")
			}
		})
	}
}

func TestProtocolCacheJSONStringSizeMatchesMarshal(t *testing.T) {
	var all strings.Builder
	for char := rune(0); char < 256; char++ {
		all.WriteRune(char)
	}
	all.WriteString("中文\u2028\u2029😀")
	for _, value := range []string{"", all.String(), "\"\\\b\f\n\r\t<>&"} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if size, ok := protocolCacheJSONStringSize(value); !ok || size != len(encoded) {
			t.Fatalf("encoded size = %d, valid = %t; want %d", size, ok, len(encoded))
		}
	}
}

func BenchmarkProtocolCatalogSnapshotAdmission(b *testing.B) {
	catalog := cacheBenchmarkCatalog()
	for _, test := range []struct {
		name  string
		clone func(any) (any, int, bool)
	}{
		{"JSONRoundTrip", cacheSnapshotViaJSON},
		{"CanonicalCopy", protocolCacheSnapshot},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, _, ok := test.clone(catalog); !ok {
					b.Fatal("benchmark catalog rejected")
				}
			}
		})
	}
}
