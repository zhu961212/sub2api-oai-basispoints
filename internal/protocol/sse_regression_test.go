package protocol

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSSEDecoderPreservesEventsAcrossChunkBoundaries(t *testing.T) {
	body := []byte(": comment\r\nevent: first\r\nevent:  update \r\ndata:  leading and trailing  \r\ndata: 中文\r\n\r\nevent: ignored\n\n: keepalive\n\ndata:\n\ndata: last\n\n")
	want := [][2]string{{"update", " leading and trailing  \n中文"}, {"", ""}, {"", "last"}}
	for size := 1; size <= len(body); size++ {
		t.Run(fmt.Sprintf("chunk_%d", size), func(t *testing.T) {
			decoder := newSSEDecoder()
			var got [][2]string
			emit := func(event, data string) error {
				got = append(got, [2]string{event, data})
				return nil
			}
			for offset := 0; offset < len(body); offset += size {
				if err := decoder.feed(body[offset:min(offset+size, len(body))], emit); err != nil {
					t.Fatal(err)
				}
				if err := decoder.feed(nil, emit); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("events = %#v, want %#v", got, want)
			}
		})
	}
}

func TestSSEDecoderOwnsDataAcrossInputReuse(t *testing.T) {
	decoder := newSSEDecoder()
	var got [][2]string
	emit := func(event, data string) error {
		got = append(got, [2]string{event, data})
		return nil
	}
	for _, input := range []string{"event: saved\ndata: par", "tial\n\n", "data: later\n\n"} {
		chunk := []byte(input)
		if err := decoder.feed(chunk, emit); err != nil {
			t.Fatal(err)
		}
		for index := range chunk {
			chunk[index] = 'x'
		}
	}
	want := [][2]string{{"saved", "partial"}, {"", "later"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("retained events = %#v, want %#v", got, want)
	}
}

func TestSSEDecoderReturnsEmitterErrorWithoutReadingNextRecord(t *testing.T) {
	decoder := newSSEDecoder()
	want := errors.New("stop downstream")
	calls := 0
	err := decoder.feed([]byte("data: first\n\ndata: second\n\n"), func(event, data string) error {
		calls++
		if data != "first" {
			t.Fatalf("emitted unexpected data %q", data)
		}
		return want
	})
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("error = %v, callbacks = %d", err, calls)
	}
}

func TestSSEDecoderLargeFragmentedDataLine(t *testing.T) {
	data := strings.Repeat("large中", (2<<20)/len("large中"))
	body := []byte("event: delta\r\ndata: " + data + "\r\n\r\n")
	decoder := newSSEDecoder()
	calls := 0
	emit := func(event, got string) error {
		calls++
		if event != "delta" || got != data {
			t.Fatalf("large event differs: event=%q data bytes=%d want=%d", event, len(got), len(data))
		}
		return nil
	}
	for offset := 0; offset < len(body); offset += 509 {
		if err := decoder.feed(body[offset:min(offset+509, len(body))], emit); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("callbacks = %d, want 1", calls)
	}
}

func TestParseFinalStreamResponseFlushesMissingRecordSeparator(t *testing.T) {
	for _, ending := range []string{"", "\n", "\r", "\r\n", "\n\n", "\r\n\r\n"} {
		t.Run(fmt.Sprintf("ending_%q", ending), func(t *testing.T) {
			body := "event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_eof\",\"status\":\"completed\",\"output\":[]}}" + ending
			response, err := ParseFinalStreamResponse([]byte(body))
			if err != nil || response["id"] != "resp_eof" {
				t.Fatalf("response = %#v, error = %v", response, err)
			}
		})
	}
}

func BenchmarkSSEDecoderFragmentedLine(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20, 8 << 20} {
		for _, chunkSize := range []int{1 << 10, 32 << 10} {
			b.Run(fmt.Sprintf("bytes_%d/chunk_%d", size, chunkSize), func(b *testing.B) {
				body := []byte("data: " + strings.Repeat("x", size) + "\n\n")
				emit := func(event, data string) error {
					if len(data) != size {
						b.Fatalf("data bytes = %d, want %d", len(data), size)
					}
					return nil
				}
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				b.ResetTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					decoder := newSSEDecoder()
					for offset := 0; offset < len(body); offset += chunkSize {
						if err := decoder.feed(body[offset:min(offset+chunkSize, len(body))], emit); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}
