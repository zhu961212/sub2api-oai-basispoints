package imagerelay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExpiryCloseAndReadOnlyHTTP(t *testing.T) {
	r := testRelay(t)
	raw := dataURL("image/png", testPNG(t))
	part := imagePart(raw)
	if _, err := r.Rewrite(request(part), "scope"); err != nil {
		t.Fatal(err)
	}
	target := part["image_url"].(string)
	token := strings.TrimPrefix(target, "https://images.example"+Path)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		w := get(r, method, target)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatal("relay accepted an upload or mutating method")
		}
	}
	for _, path := range []string{Path, Path + "../PRIVATE", Path + strings.Repeat("a", 43), Path + token + "/PRIVATE"} {
		w := get(r, http.MethodHead, "https://images.example"+path)
		if w.Code != http.StatusNotFound || w.Body.Len() != 0 {
			t.Fatal("invalid capability was exposed")
		}
	}
	for i := 0; i < maxDownloads; i++ {
		r.downloads <- struct{}{}
	}
	w := get(r, http.MethodGet, target)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Fatal("download concurrency not bounded")
	}
	for i := 0; i < maxDownloads; i++ {
		<-r.downloads
	}
	r.mu.Lock()
	r.entries[token].expires = time.Now().Add(-time.Second)
	r.mu.Unlock()
	if get(r, http.MethodHead, target).Code != http.StatusNotFound {
		t.Fatal("expired image remains accessible")
	}
	assertEmpty(t, r)
	part = imagePart(raw)
	if _, err := r.Rewrite(request(part), "scope"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(r.root, "unrelated.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := r.dir
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("close left session files behind")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatal("close affected unrelated storage")
	}
	if _, err := r.Rewrite(request(imagePart(raw)), "scope"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("closed relay accepted rewrite")
	}
	if err := r.SetPublicOrigin("https://new.example"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("closed relay accepted origin update")
	}
	if get(r, http.MethodGet, target).Code != http.StatusNotFound {
		t.Fatal("closed relay served an image")
	}
	if err := r.Close(); err != nil {
		t.Fatal("second close should be harmless")
	}
}

func TestOrphanCleanupPreservesLiveAndUnownedDirectories(t *testing.T) {
	root := t.TempDir()
	first, err := New("https://images.example", root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	stale := time.Now().Add(-imageTTL - 2*cleanupInterval)
	if err := os.Chtimes(first.dir, stale, stale); err != nil {
		t.Fatal(err)
	}
	crashed := filepath.Join(root, "session-crashed")
	unowned := filepath.Join(root, "session-unowned")
	for _, dir := range []string{crashed, unowned} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(crashed, leaseName), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crashed, "image-old"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{crashed, unowned} {
		if err := os.Chtimes(dir, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	second, err := New("https://images.example", root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := os.Stat(first.dir); err != nil {
		t.Fatal("stale live process session was removed")
	}
	if _, err := os.Stat(unowned); err != nil {
		t.Fatal("unowned directory was removed")
	}
	if _, err := os.Stat(crashed); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("crashed process session was not reclaimed")
	}
}

type blockedWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedWriter) Header() http.Header { return w.header }
func (w *blockedWriter) WriteHeader(int)     {}
func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}
func waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
}

func TestCloseWaitsForInFlightDownload(t *testing.T) {
	r := testRelay(t)
	part := imagePart(dataURL("image/png", testPNG(t)))
	if _, err := r.Rewrite(request(part), "scope"); err != nil {
		t.Fatal(err)
	}
	w := &blockedWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	downloadDone := make(chan struct{})
	go func() {
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, part["image_url"].(string), nil))
		close(downloadDone)
	}()
	waitEntered(t, w.entered)
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("close returned before download finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(w.release)
	<-downloadDone
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not finish")
	}
}

func TestExpiredDownloadKeepsReplacementImageAndQuota(t *testing.T) {
	r := testRelay(t)
	data := testPNG(t)
	raw := dataURL("image/png", data)
	part := imagePart(raw)
	if _, err := r.Rewrite(request(part), "scope"); err != nil {
		t.Fatal(err)
	}
	target := part["image_url"].(string)
	token := strings.TrimPrefix(target, "https://images.example"+Path)
	w := &blockedWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil)); close(done) }()
	waitEntered(t, w.entered)
	r.mu.Lock()
	old := r.entries[token]
	old.expires = time.Now().Add(-time.Second)
	r.usedImages, r.usedBytes = maxStorageImages, maxStorageBytes
	r.mu.Unlock()
	replacement := imagePart(raw)
	if _, err := r.Rewrite(request(replacement), "scope"); err != nil {
		close(w.release)
		t.Fatal(err)
	}
	r.mu.Lock()
	newImage := r.entries[token]
	if newImage != old || r.usedImages != maxStorageImages || r.usedBytes != maxStorageBytes || len(r.retired) != 0 {
		t.Error("expired in-flight image was not reused at full capacity")
	}
	r.mu.Unlock()
	close(w.release)
	<-done
	r.mu.Lock()
	if r.entries[token] != newImage || r.usedImages != maxStorageImages || r.usedBytes != maxStorageBytes || len(r.retired) != 0 {
		t.Error("expired reader damaged revived image or quota")
	}
	r.usedImages, r.usedBytes = 1, int64(len(data))
	r.mu.Unlock()
	if get(r, http.MethodGet, replacement["image_url"].(string)).Code != http.StatusOK {
		t.Fatal("replacement image unavailable")
	}
}
