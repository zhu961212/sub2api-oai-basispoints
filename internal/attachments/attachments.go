// Package attachments uploads message images as native attachments and validates
// inline tool screenshots without a public URL, temporary file, or local listener.
package attachments

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxImageBytes    = 20 << 20
	maxRequestBytes  = 32 << 20
	maxRequestImages = 128
	maxPixels        = 64 * 1024 * 1024
	maxCacheEntries  = 512
	maxUploads       = 32
	maxResponseBytes = 64 << 10
	cacheTTL         = 30 * time.Minute
)

// Error never retains upstream bodies, image bytes, URLs, or credentials.
type Error struct {
	status  int
	code    string
	message string
	cause   error
	reason  string
}

func (e *Error) Error() string   { return e.message }
func (e *Error) Unwrap() error   { return e.cause }
func (e *Error) StatusCode() int { return e.status }
func (e *Error) Code() string    { return e.code }

// DiagnosticReason exposes only fixed transport classifications, never the
// original error, URL, certificate, or account details. Invalid responses only
// expose a classification when reading their body failed with a known cause.
func (e *Error) DiagnosticReason() string {
	if e == nil || (e.code != "attachment_transport" && e.code != "invalid_attachment_response") {
		return ""
	}
	if e.code == "invalid_attachment_response" && e.reason == "generic" {
		return ""
	}
	switch e.reason {
	case "timeout", "dns", "tls", "dial", "connection_closed", "generic":
		return e.reason
	default:
		return ""
	}
}

func fail(status int, code, message string) error {
	return &Error{status: status, code: code, message: message}
}

func canceled(err error) error {
	return &Error{status: 499, code: "attachment_canceled", message: "Basis Points attachment request was canceled", cause: err}
}

type cacheEntry struct {
	fileID  string
	expires time.Time
	used    uint64
}

type pendingUpload struct {
	done   chan struct{}
	fileID string
	err    error
}

// Uploader only retains bounded file-ID metadata and keyed digests. It needs
// no Close method or background cleaner and never retains image payloads.
type Uploader struct {
	mu       sync.Mutex
	key      [32]byte
	cache    map[[32]byte]cacheEntry
	pending  map[[32]byte]*pendingUpload
	sequence uint64
	uploads  chan struct{}
	now      func() time.Time
}

func New() *Uploader {
	u := &Uploader{cache: make(map[[32]byte]cacheEntry), pending: make(map[[32]byte]*pendingUpload), uploads: make(chan struct{}, maxUploads), now: time.Now}
	_, _ = rand.Read(u.key[:])
	return u
}

type inlineImage struct {
	mime    string
	payload string
	size    int64
	key     [32]byte
	// Request-local state: a tool screenshot may be seen before the same image
	// appears in a message that needs an authenticated upload/cache key.
	keyReady  bool
	validated bool
}

type imageEdit struct {
	part   map[string]any
	image  *inlineImage
	upload bool
}

// Rewrite visits typed images in messages and function/custom tool results.
// All images validate before any upload; source changes only after every upload
// succeeds. Only message images become file IDs: tool screenshots retain their
// data URLs. An empty scope disables reuse across requests. Caller maps must not
// be accessed concurrently. Existing HTTPS references and file IDs are untouched.
// A validated inline tool screenshot returns true even if its detail was already
// explicit, so callers serialize the validated request and use image-safe errors.
func (u *Uploader) Rewrite(ctx context.Context, client *http.Client, responsesURL string, headers http.Header, source map[string]any, scope string) (bool, error) {
	var edits []imageEdit
	unique := make(map[[32]byte]*inlineImage)
	var encoded map[string]*inlineImage
	var firstRaw string
	var firstImage *inlineImage
	var order []*inlineImage
	var total int64
	endpoint := ""
	items, _ := source["input"].([]any)
	for i, rawItem := range items {
		item, _ := rawItem.(map[string]any)
		field := ""
		switch item["type"] {
		case nil, "", "message":
			field = "content"
		case "function_call_output", "custom_tool_call_output":
			field = "output"
		default:
			continue
		}
		parts, _ := item[field].([]any)
		for j, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part["type"] != "input_image" {
				continue
			}
			raw, _ := part["image_url"].(string)
			if len(raw) < 5 || !strings.EqualFold(raw[:5], "data:") {
				continue
			}
			if err := ctx.Err(); err != nil {
				return false, canceled(err)
			}
			upload := field == "content"
			if upload {
				if u == nil || client == nil {
					return false, fail(503, "attachment_unavailable", "Basis Points attachment transport is unavailable")
				}
				if endpoint == "" {
					var err error
					endpoint, err = attachmentURL(responsesURL)
					if err != nil {
						return false, err
					}
				}
			}
			path := fmt.Sprintf("input[%d].%s[%d]", i, field, j)
			invalid := func(message string) error { return fail(400, "invalid_image", message+" (path="+path+")") }
			if len(edits) >= maxRequestImages {
				return false, invalid(fmt.Sprintf("The plugin allows at most %d inline images per request; received at least %d. Conversation history, tool screenshots, and repeated images all count toward this limit; start a new conversation or use fewer images", maxRequestImages, len(edits)+1))
			}
			if value, exists := part["file_id"]; exists && value != nil && value != "" {
				return false, invalid("input_image cannot contain both image_url and file_id")
			}
			img := firstImage
			if raw != firstRaw {
				img = encoded[raw]
			}
			if img == nil {
				var err error
				img, err = parseImageMetadata(raw)
				if err != nil {
					return false, invalid(err.Error())
				}
			}
			if upload && !img.keyReady {
				var err error
				img.key, err = u.imageKey(ctx, endpoint, scope, headers, img)
				if err != nil {
					return false, err
				}
				if existing := unique[img.key]; existing != nil {
					img = existing
				} else {
					img.keyReady = true
					unique[img.key] = img
					order = append(order, img)
				}
			}
			if !img.validated {
				// Only a full authenticated upload-cache hit can skip scans.
				// Tool-only images always validate without an upload dependency.
				if !upload || scope == "" || u.cached(img.key) == "" {
					if err := validateImage(ctx, img); err != nil {
						if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
							return false, canceled(err)
						}
						return false, invalid(err.Error())
					}
				}
				img.validated = true
			}
			// Request-local references only; payloads never enter the
			// persistent cache. Duplicate blocks avoid repeated scans/HMACs.
			if firstImage == nil || raw == firstRaw {
				firstRaw, firstImage = raw, img
			} else {
				if encoded == nil {
					encoded = make(map[string]*inlineImage)
				}
				encoded[raw] = img
			}
			total += img.size
			if total > maxRequestBytes {
				return false, invalid(fmt.Sprintf("Inline images exceed the 32 MiB request limit; counted %.2f MiB across %d images. Conversation history, tool screenshots, and repeated images all count toward this limit; start a new conversation or use fewer/smaller images", float64(total)/(1<<20), len(edits)+1))
			}
			edits = append(edits, imageEdit{part: part, image: img, upload: upload})
		}
	}
	if len(edits) == 0 {
		return false, nil
	}
	ids := make(map[[32]byte]string, len(order))
	for _, img := range order {
		id, err := u.getOrUpload(ctx, img.key, scope != "", func() (string, error) { return u.upload(ctx, client, endpoint, headers, img) })
		if err != nil {
			return false, err
		}
		ids[img.key] = id
	}
	if err := ctx.Err(); err != nil {
		return false, canceled(err)
	}
	for _, edit := range edits {
		if edit.upload {
			delete(edit.part, "image_url")
			edit.part["file_id"] = ids[edit.image.key]
		}
		if edit.part["detail"] == nil {
			edit.part["detail"] = "auto"
		}
	}
	return true, nil
}

func (u *Uploader) cached(key [32]byte) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cachedLocked(key)
}

// cachedLocked requires u.mu. Keep lookup, expiry, and recency updates atomic
// with pending-upload registration so misses do not need a second lookup.
func (u *Uploader) cachedLocked(key [32]byte) string {
	entry, ok := u.cache[key]
	if !ok {
		return ""
	}
	if !u.now().Before(entry.expires) {
		delete(u.cache, key)
		return ""
	}
	u.sequence++
	entry.used = u.sequence
	u.cache[key] = entry
	return entry.fileID
}

func (u *Uploader) getOrUpload(ctx context.Context, key [32]byte, reusable bool, upload func() (string, error)) (string, error) {
	return u.getOrUploadAttempt(ctx, key, reusable, upload, true)
}

func (u *Uploader) getOrUploadAttempt(ctx context.Context, key [32]byte, reusable bool, upload func() (string, error), retryCanceledOwner bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", canceled(err)
	}
	if !reusable {
		return upload()
	}
	u.mu.Lock()
	if id := u.cachedLocked(key); id != "" {
		u.mu.Unlock()
		return id, nil
	}
	if pending := u.pending[key]; pending != nil {
		u.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", canceled(ctx.Err())
		case <-pending.done:
			if err := ctx.Err(); err != nil {
				return "", canceled(err)
			}
			// The creator owns the shared HTTP request context. Its cancellation
			// must not cancel a still-active waiter. Recompete once using this
			// caller's upload closure/context, never retrying ordinary failures
			// or entering an unbounded chain of canceled owners.
			if retryCanceledOwner && (errors.Is(pending.err, context.Canceled) || errors.Is(pending.err, context.DeadlineExceeded)) {
				return u.getOrUploadAttempt(ctx, key, reusable, upload, false)
			}
			return pending.fileID, pending.err
		}
	}
	if len(u.pending) >= maxUploads {
		u.mu.Unlock()
		return "", fail(503, "attachment_busy", "Basis Points attachment uploads are busy; retry shortly")
	}
	pending := &pendingUpload{done: make(chan struct{})}
	u.pending[key] = pending
	u.mu.Unlock()
	id, err := upload()
	u.mu.Lock()
	delete(u.pending, key)
	if err == nil {
		now := u.now()
		// Expired entries are rejected on lookup. Reclaim unrelated expired
		// metadata only when space is needed, rather than scanning the whole
		// cache after every upload. One pass also finds the oldest live entry.
		if len(u.cache) >= maxCacheEntries {
			var oldest [32]byte
			oldestUsed := ^uint64(0)
			for key, value := range u.cache {
				if !now.Before(value.expires) {
					delete(u.cache, key)
					continue
				}
				if value.used < oldestUsed {
					oldest, oldestUsed = key, value.used
				}
			}
			if len(u.cache) >= maxCacheEntries {
				delete(u.cache, oldest)
			}
		}
		u.sequence++
		u.cache[key] = cacheEntry{fileID: id, expires: now.Add(cacheTTL), used: u.sequence}
	}
	pending.fileID, pending.err = id, err
	close(pending.done)
	u.mu.Unlock()
	return id, err
}
