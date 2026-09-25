package transport

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSSERelayDecoderChunkBoundariesAndOwnership(t *testing.T) {
	first := ": keepalive\r\nevent: first\r\nevent: response.output_text.delta\r\nunknown: retained\r\ndata: 文🙂\r\ndata:  second\r\n\r\n"
	second := "data: [DONE]\n\n"
	body := []byte(first + second)
	want := []sseRelayEvent{
		{raw: []byte(first), event: "response.output_text.delta", data: "文🙂\n second"},
		{raw: []byte(second), data: "[DONE]"},
	}
	for split := 0; split <= len(body); split++ {
		var decoder sseRelayDecoder
		var got []sseRelayEvent
		emit := func(event sseRelayEvent) error { got = append(got, event); return nil }
		for _, part := range [][]byte{body[:split], body[split:]} {
			chunk := bytes.Clone(part)
			if err := decoder.feed(chunk, emit); err != nil {
				t.Fatal(err)
			}
			// HTTP reuses its read buffer; emitted and partial data must own bytes.
			for i := range chunk {
				chunk[i] = 'x'
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d: got %#v, want %#v", split, got, want)
		}
	}
}

func TestSSERelayDecoderPartialLineAndEOFFlush(t *testing.T) {
	var decoder sseRelayDecoder
	var got []sseRelayEvent
	emit := func(event sseRelayEvent) error { got = append(got, event); return nil }
	for _, chunk := range [][]byte{[]byte("event: final\r\ndata: par"), nil, []byte("tial")} {
		if err := decoder.feed(chunk, emit); err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatal("partial record emitted before EOF flush")
		}
	}
	if err := decoder.feed([]byte("\n\n"), emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].event != "final" || got[0].data != "partial" || string(got[0].raw) != "event: final\r\ndata: partial\n\n" {
		t.Fatalf("EOF flush changed partial record: %#v", got)
	}
}

func TestSSERelayDecoderStopsAtEmitError(t *testing.T) {
	want := errors.New("downstream failed")
	calls := 0
	var decoder sseRelayDecoder
	err := decoder.feed([]byte("data: first\n\ndata: second\n\n"), func(sseRelayEvent) error { calls++; return want })
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("error = %v, callback count = %d", err, calls)
	}
}

func TestSSERelayDecoderLargeFragmentedLineAllocations(t *testing.T) {
	body := []byte("data: " + strings.Repeat("x", 1<<20) + "\n\n")
	allocations := testing.AllocsPerRun(2, func() {
		var decoder sseRelayDecoder
		count := 0
		emit := func(event sseRelayEvent) error {
			count++
			if len(event.data) != 1<<20 || !bytes.Equal(event.raw, body) {
				t.Fatal("large record changed")
			}
			return nil
		}
		for offset := 0; offset < len(body); offset += 4 << 10 {
			if err := decoder.feed(body[offset:min(offset+(4<<10), len(body))], emit); err != nil {
				t.Fatal(err)
			}
		}
		if count != 1 {
			t.Fatalf("emitted %d records, want 1", count)
		}
	})
	// Growth may allocate logarithmically; copying each partial line allocates
	// once per fragment (over 250 times for this fixture). Leave ample slack.
	if allocations > 64 {
		t.Fatalf("fragmented line used %.0f allocations, want <= 64", allocations)
	}
}

func BenchmarkSSERelayDecoderFragmented(b *testing.B) {
	for _, size := range []int{1 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			body := []byte("data: " + strings.Repeat("x", size) + "\n\n")
			emit := func(event sseRelayEvent) error {
				if len(event.data) != size {
					b.Fatal("decoded size changed")
				}
				return nil
			}
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var decoder sseRelayDecoder
				for offset := 0; offset < len(body); offset += 32 << 10 {
					if err := decoder.feed(body[offset:min(offset+(32<<10), len(body))], emit); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
