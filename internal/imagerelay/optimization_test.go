package imagerelay

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestInlineDecodedSizeAndCanonicalPadding(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    int64
	}{
		{"AA==", 1}, {"AAA=", 2}, {"AAAA", 3}, {"AAAAAA==", 4},
		{"AB==", 0}, {"AAB=", 0}, {"AA", 0}, {"AAA", 0},
		{"A===", 0}, {"AA==AAAA", 0}, {"AAAA=AAA", 0}, {"", 0},
	} {
		n, err := inlineDecodedSize(tc.payload)
		if tc.want == 0 {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid padding accepted: %q", tc.payload)
			}
		} else if err != nil || n != tc.want {
			t.Fatalf("size for %q = %d %v; want %d", tc.payload, n, err, tc.want)
		}
	}
	// 20 MiB and 20 MiB+1 have the same padded encoded length.
	prefix := strings.Repeat("A", base64.StdEncoding.EncodedLen(maxImageBytes)-4)
	if n, err := inlineDecodedSize(prefix + "AAA="); err != nil || n != maxImageBytes {
		t.Fatalf("exact image limit rejected: %d %v", n, err)
	}
	if _, err := inlineDecodedSize(prefix + "AAAA"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("image limit exceeded via padding: %v", err)
	}
}

func TestCachedImageStillRejectsDifferentMIMEAndMalformedPayload(t *testing.T) {
	r := testRelay(t)
	data := testPNG(t)
	valid := dataURL("image/png", data)
	part := imagePart(valid)
	if _, err := r.Rewrite(request(part), "scope"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		dataURL("image/jpeg", data), valid + "AAAA", valid + "!",
		strings.Replace(valid, "base64,", "base64,!", 1),
	} {
		if changed, err := r.Rewrite(request(imagePart(raw)), "scope"); changed || !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid cached image accepted: %v %v", changed, err)
		}
	}
	if r.usedImages != 1 || r.usedBytes != int64(len(data)) {
		t.Fatal("invalid reuse changed storage accounting")
	}
	w := get(r, http.MethodGet, part["image_url"].(string))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("invalid reuse damaged the original image")
	}
}

func TestInlineBase64RejectsPaddingAcrossDecoderBlocks(t *testing.T) {
	for _, size := range []int{766, 767, 24574, 24575} {
		r := testRelay(t)
		data := make([]byte, size)
		copy(data, testPNG(t))
		raw := dataURL("image/png", data) + "AAAA"
		if changed, err := r.Rewrite(request(imagePart(raw)), "scope"); changed || !errors.Is(err, ErrInvalid) {
			t.Fatalf("interior padding accepted at size %d: %v %v", size, changed, err)
		}
		assertEmpty(t, r)
	}
}

func TestInlineBase64InvalidCharacterAtHashBoundaries(t *testing.T) {
	r := testRelay(t)
	data := make([]byte, 64<<10)
	copy(data, testPNG(t))
	payload := base64.StdEncoding.EncodeToString(data)
	for _, pos := range []int{0, 1023, 1024, 32767, 32768, 65535, 65536, len(payload) - 4} {
		broken := payload[:pos] + "!" + payload[pos+1:]
		raw := "data:image/png;base64," + broken
		if changed, err := r.Rewrite(request(imagePart(raw)), "scope"); changed || !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid byte accepted at %d: %v %v", pos, changed, err)
		}
		assertEmpty(t, r)
	}
}

func TestFullRelayStillValidatesBase64(t *testing.T) {
	r := testRelay(t)
	r.usedImages = maxStorageImages
	defer func() { r.usedImages = 0 }()
	data := make([]byte, 2049)
	copy(data, testPNG(t))
	payload := base64.StdEncoding.EncodeToString(data)
	payload = payload[:100] + "!" + payload[101:]
	if _, err := r.Rewrite(request(imagePart("data:image/png;base64,"+payload)), "scope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("capacity masked malformed base64: %v", err)
	}
	if len(r.images) != 0 || len(r.entries) != 0 || r.usedBytes != 0 || r.usedImages != maxStorageImages {
		t.Fatal("failed validation changed capacity")
	}
}

func TestConcurrentInvalidImagesReleaseReservations(t *testing.T) {
	r := testRelay(t)
	data := make([]byte, 64<<10)
	copy(data, testPNG(t))
	payload := base64.StdEncoding.EncodeToString(data)
	broken := "data:image/png;base64," + payload[:32768] + "!" + payload[32769:]
	start := make(chan struct{})
	errs := make(chan error, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := r.Rewrite(request(imagePart(broken)), "scope")
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("concurrent malformed image: %v", err)
		}
	}
	assertEmpty(t, r)
}
