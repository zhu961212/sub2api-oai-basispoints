package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func cacheBenchmarkCatalog() []any {
	tools := make([]any, 64)
	for index := range tools {
		tools[index] = map[string]any{"type": "function", "name": fmt.Sprintf("tool_%d", index),
			"description": strings.Repeat("Read scoped workspace state. ", 20),
			"parameters": map[string]any{"type": "object", "properties": map[string]any{
				"path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "maximum": json.Number("9007199254740993")},
			}},
		}
	}
	return tools
}

func BenchmarkProtocolCatalogCacheHit(b *testing.B) {
	source := map[string]any{"session_id": b.Name(), "tools": cacheBenchmarkCatalog()}
	rememberToolCatalog(source, source["tools"])
	for _, parallel := range []bool{false, true} {
		b.Run(fmt.Sprintf("parallel=%t", parallel), func(b *testing.B) {
			b.ReportAllocs()
			if parallel {
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						_ = rememberedToolCatalog(source)
					}
				})
			} else {
				for range b.N {
					_ = rememberedToolCatalog(source)
				}
			}
		})
	}
}

func BenchmarkProtocolNativeCacheHit(b *testing.B) {
	namespace := b.Name()
	native := sessionTestNative("fc_benchmark")
	native["references"] = cacheBenchmarkCatalog()
	rememberNativeCallInNamespace(namespace, native)
	b.ReportAllocs()
	for range b.N {
		_ = rememberedNativeCallInNamespace(namespace, "shared_call")
	}
}
