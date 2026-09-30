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
	source := map[string]any{"__bps_session_scope": b.Name(), "tools": cacheBenchmarkCatalog()}
	rememberToolCatalog(source, source["tools"])
	b.Cleanup(func() { toolCatalogCache.forget(cacheNamespaceForSource(source)) })
	if tools, ok := rememberedToolCatalog(source).([]any); !ok || len(tools) != 64 {
		b.Fatal("benchmark catalog cache did not warm up")
	}
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
	b.Cleanup(func() { nativeCallCache.forget(nativeCacheKey(namespace, "shared_call")) })
	if got := rememberedNativeCallInNamespace(namespace, "shared_call"); got == nil {
		b.Fatal("benchmark native cache did not warm up")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = rememberedNativeCallInNamespace(namespace, "shared_call")
	}
}
