// Package imagerelay turns inline Responses images into short-lived HTTPS URLs.
// Its HTTP handler only serves images; the embedding process exposes the route.
package imagerelay

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	Path             = "/api/bps-images/"
	imageTTL         = 30 * time.Minute
	cleanupInterval  = time.Minute
	maxImageBytes    = 20 << 20
	maxRequestBytes  = 32 << 20
	maxRequestImages = 20
	maxPixels        = 64 * 1024 * 1024
	maxStorageBytes  = 1 << 30
	maxStorageImages = 512
	maxDownloads     = 32
	leaseName        = ".lease"
)

var (
	ErrInvalid     = errors.New("invalid image relay request")
	ErrFull        = errors.New("image relay capacity is full")
	ErrUnavailable = errors.New("image relay storage is unavailable")
)

type storedImage struct {
	token       string
	path        string
	contentType string
	size        int64
	charge      int64
	expires     time.Time
	readers     int
	pins        int
	ready       chan struct{}
	stageErr    error
}

// Relay keeps image bytes in private disk files. Quotas include staged files
// and files still open by downloads after their URL expires.
type Relay struct {
	mu          sync.Mutex
	origin      string
	root        string
	dir         string
	lease       *os.File
	key         [32]byte
	entries     map[string]*storedImage
	images      map[string]*storedImage
	retired     map[*storedImage]bool
	usedBytes   int64
	usedImages  int
	closed      bool
	closeErr    error
	operations  sync.WaitGroup
	stop        chan struct{}
	cleanerDone chan struct{}
	closeDone   chan struct{}
	downloads   chan struct{}
}

// ValidatePublicOrigin checks syntax. The embedding process must make Path
// reachable on this HTTPS origin by the upstream.
func ValidatePublicOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || strings.TrimSpace(origin) != origin || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(origin, "#") {
		return invalid("public origin must be HTTPS without credentials, path, query or fragment")
	}
	return nil
}

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

func New(publicOrigin, storageRoot string) (*Relay, error) {
	if err := ValidatePublicOrigin(publicOrigin); err != nil {
		return nil, err
	}
	if strings.TrimSpace(storageRoot) == "" {
		return nil, ErrUnavailable
	}
	root, err := filepath.Abs(storageRoot)
	if err != nil || os.MkdirAll(root, 0700) != nil {
		return nil, ErrUnavailable
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || os.Chmod(root, 0700) != nil {
		return nil, ErrUnavailable
	}
	r := &Relay{origin: strings.TrimSuffix(publicOrigin, "/"), root: root,
		entries: make(map[string]*storedImage), images: make(map[string]*storedImage), retired: make(map[*storedImage]bool),
		stop: make(chan struct{}), cleanerDone: make(chan struct{}), closeDone: make(chan struct{}), downloads: make(chan struct{}, maxDownloads)}
	if _, err := rand.Read(r.key[:]); err != nil {
		return nil, ErrUnavailable
	}
	r.dir, err = os.MkdirTemp(root, "session-")
	if err != nil {
		return nil, ErrUnavailable
	}
	if os.Chmod(r.dir, 0700) != nil {
		_ = os.RemoveAll(r.dir)
		return nil, ErrUnavailable
	}
	r.lease, err = os.OpenFile(filepath.Join(r.dir, leaseName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		_ = os.RemoveAll(r.dir)
		return nil, ErrUnavailable
	}
	if lockLease(r.lease) != nil {
		_ = r.lease.Close()
		_ = os.RemoveAll(r.dir)
		return nil, ErrUnavailable
	}
	r.cleanupOrphans(time.Now())
	go r.cleanupLoop()
	return r, nil
}

func (r *Relay) SetPublicOrigin(origin string) error {
	if err := ValidatePublicOrigin(origin); err != nil {
		return err
	}
	if r == nil {
		return ErrUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrUnavailable
	}
	r.origin = strings.TrimSuffix(origin, "/")
	return nil
}

// Close waits for active rewrites/downloads, then removes only its own session.
// Concurrent Close calls wait for the same cleanup result.
func (r *Relay) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.closeDone
		return r.closeErr
	}
	r.closed = true
	close(r.stop)
	r.mu.Unlock()
	<-r.cleanerDone
	r.operations.Wait()
	leaseErr := unlockLease(r.lease)
	fileErr := r.lease.Close()
	removeErr := os.RemoveAll(r.dir)
	if leaseErr != nil || fileErr != nil || removeErr != nil {
		r.closeErr = ErrUnavailable
	}
	close(r.closeDone)
	return r.closeErr
}

type imageEdit struct {
	part  map[string]any
	image *storedImage
	token string
}

// Rewrite visits typed input_image blocks in message content and function/custom
// tool outputs. It preserves numbers, tool arguments and HTTPS references.
// No caller map changes until all inline images validate. The caller must not
// concurrently access source. Image detail is preserved for protocol validation.
func (r *Relay) Rewrite(source map[string]any, scope string) (bool, error) {
	if r == nil {
		return false, ErrUnavailable
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false, ErrUnavailable
	}
	r.operations.Add(1)
	origin := r.origin
	r.mu.Unlock()
	defer r.operations.Done()
	var edits []imageEdit
	// Request-local only: do not retain base64 payloads beyond this rewrite.
	staged := make(map[string]*storedImage)
	var total int64
	committed := false
	defer func() {
		if !committed {
			for _, edit := range edits {
				r.releaseImage(edit.image)
			}
		}
	}()
	items, _ := source["input"].([]any)
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		var content any
		switch item["type"] {
		case nil, "", "message":
			content = item["content"]
		case "function_call_output", "custom_tool_call_output":
			content = item["output"]
		default:
			continue
		}
		parts, _ := content.([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part["type"] != "input_image" {
				continue
			}
			raw, _ := part["image_url"].(string)
			if len(raw) < 5 || !strings.EqualFold(raw[:5], "data:") {
				continue
			}
			if len(edits) >= maxRequestImages {
				return false, invalid("at most 20 inline images are allowed per request")
			}
			if fileID, exists := part["file_id"]; exists && fileID != nil && fileID != "" {
				return false, invalid("inline images cannot also use file_id")
			}
			img := staged[raw]
			if img == nil {
				var err error
				img, _, err = r.stageImage(raw, scope)
				if err != nil {
					return false, err
				}
				staged[raw] = img
			} else {
				// The first occurrence holds a pin until the atomic commit.
				r.mu.Lock()
				img.pins++
				r.mu.Unlock()
			}
			edits = append(edits, imageEdit{part: part, image: img, token: img.token})
			total += img.size
			if total > maxRequestBytes {
				return false, invalid("inline images exceed the 32 MiB request limit")
			}
		}
	}
	if len(edits) == 0 {
		return false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false, ErrUnavailable
	}
	now := time.Now()
	r.pruneLocked(now)
	for _, edit := range edits {
		edit.image.expires = now.Add(imageTTL)
		r.entries[edit.token] = edit.image
		delete(r.retired, edit.image)
		edit.image.pins--
		edit.part["image_url"] = origin + Path + edit.token
	}
	committed = true
	return true, nil
}
