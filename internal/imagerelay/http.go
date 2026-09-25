package imagerelay

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	token, ok := strings.CutPrefix(req.URL.Path, Path)
	if r == nil || !ok || len(token) != 43 || !validToken(token) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	select {
	case r.downloads <- struct{}{}:
		defer func() { <-r.downloads }()
	default:
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	r.mu.Lock()
	img := r.entries[token]
	expired := img != nil && !time.Now().Before(img.expires)
	if r.closed || img == nil || expired {
		if expired {
			delete(r.entries, token)
			r.removeLocked(img)
		}
		r.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	file, err := os.Open(img.path)
	if err != nil {
		delete(r.entries, token)
		r.removeLocked(img)
		r.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	img.readers++
	r.operations.Add(1)
	r.mu.Unlock()
	defer func() {
		_ = file.Close()
		r.mu.Lock()
		img.readers--
		if r.retired[img] {
			r.removeLocked(img)
		}
		r.mu.Unlock()
		r.operations.Done()
	}()
	w.Header().Set("Content-Type", img.contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(img.size, 10))
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodGet {
		// Let the file/HTTP writer use their optimized transfer path without
		// allocating a scratch buffer that WriterTo/ReaderFrom would ignore.
		_, _ = io.Copy(w, file)
	}
}

func validToken(token string) bool {
	for _, c := range token {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
