package imagerelay

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReplayAtCapacityDoesNotReserveExtraStorage(t *testing.T) {
	for _, fullByBytes := range []bool{false, true} {
		name := "entry-limit"
		if fullByBytes {
			name = "byte-limit"
		}
		t.Run(name, func(t *testing.T) {
			r := testRelay(t)
			raw := dataURL("image/png", testPNG(t))
			part := imagePart(raw)
			if _, err := r.Rewrite(request(part), "scope"); err != nil {
				t.Fatal(err)
			}
			target := part["image_url"].(string)
			token := strings.TrimPrefix(target, "https://images.example"+Path)
			r.mu.Lock()
			if fullByBytes {
				r.usedBytes = maxStorageBytes
			} else {
				r.usedImages = maxStorageImages
			}
			beforeBytes, beforeImages := r.usedBytes, r.usedImages
			r.mu.Unlock()
			first, second := imagePart(raw), imagePart(raw)
			if _, err := r.Rewrite(request(first, second), "scope"); err != nil {
				t.Fatalf("existing image replay at capacity failed: %v", err)
			}
			if first["image_url"] != target || second["image_url"] != target {
				t.Fatal("existing capability changed")
			}
			if r.usedBytes != beforeBytes || r.usedImages != beforeImages || len(r.entries) != 1 || len(r.images) != 1 || r.entries[token].pins != 0 {
				t.Fatal("replay reserved storage or leaked a pin")
			}
			files, err := os.ReadDir(r.dir)
			if err != nil || len(files) != 2 {
				t.Fatal("replay left extra disk files")
			}
			if _, err := r.Rewrite(request(imagePart(raw)), "different-scope"); !errors.Is(err, ErrFull) {
				t.Fatal("new scoped image exceeded quota")
			}
			if get(r, http.MethodGet, target).Code != http.StatusOK {
				t.Fatal("full capacity damaged existing image")
			}
		})
	}
}

func TestReusedImageRollbackPreservesTTLAndExistingFiles(t *testing.T) {
	r := testRelay(t)
	raw := dataURL("image/png", testPNG(t))
	part := imagePart(raw)
	if _, err := r.Rewrite(request(part), "scope"); err != nil {
		t.Fatal(err)
	}
	target := part["image_url"].(string)
	token := strings.TrimPrefix(target, "https://images.example"+Path)
	expires := time.Now().Add(time.Minute)
	r.mu.Lock()
	r.entries[token].expires = expires
	r.usedImages = maxStorageImages
	r.mu.Unlock()
	source := request(imagePart(raw), imagePart(raw), imagePart("data:image/png;base64,PRIVATE_INVALID"))
	before, _ := json.Marshal(source)
	if changed, err := r.Rewrite(source, "scope"); changed || !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid reuse request returned %v %v", changed, err)
	}
	after, _ := json.Marshal(source)
	if string(before) != string(after) {
		t.Fatal("failed reuse request modified source")
	}
	r.mu.Lock()
	img := r.entries[token]
	if img == nil || !img.expires.Equal(expires) || img.pins != 0 || r.usedImages != maxStorageImages || len(r.retired) != 0 {
		t.Error("rollback renewed TTL or discarded existing storage")
	}
	r.mu.Unlock()
	if get(r, http.MethodGet, target).Code != http.StatusOK {
		t.Fatal("rollback removed existing image")
	}
}

func TestDuplicateImagesShareLastAvailableSlot(t *testing.T) {
	r := testRelay(t)
	raw := dataURL("image/png", testPNG(t))
	r.mu.Lock()
	r.usedImages = maxStorageImages - 1
	r.mu.Unlock()
	first, second := imagePart(raw), imagePart(raw)
	if _, err := r.Rewrite(request(first, second), "scope"); err != nil {
		t.Fatalf("duplicate request consumed more than one slot: %v", err)
	}
	if first["image_url"] != second["image_url"] || r.usedImages != maxStorageImages || len(r.images) != 1 || len(r.entries) != 1 {
		t.Fatal("duplicate request did not share one file")
	}
}

func TestConcurrentDuplicatesShareLastAvailableSlot(t *testing.T) {
	r := testRelay(t)
	raw := dataURL("image/png", testPNG(t))
	r.mu.Lock()
	r.usedImages = maxStorageImages - 1
	r.mu.Unlock()
	start := make(chan struct{})
	errs := make(chan error, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := r.Rewrite(request(imagePart(raw), imagePart(raw)), "scope")
			if err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if r.usedImages != maxStorageImages || len(r.images) != 1 || len(r.entries) != 1 {
		t.Fatal("concurrent duplicates reserved more than one slot")
	}
	for _, img := range r.images {
		if img.pins != 0 {
			t.Error("rewrite left pinned storage")
		}
	}
}
