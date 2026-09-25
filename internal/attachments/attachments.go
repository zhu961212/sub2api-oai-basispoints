// Package attachments sends inline images to the upstream native attachment
// endpoint, without a public download URL, temporary file, or local listener.
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
	maxRequestImages = 20
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
}

func (e *Error) Error() string   { return e.message }
func (e *Error) Unwrap() error   { return e.cause }
func (e *Error) StatusCode() int { return e.status }
func (e *Error) Code() string    { return e.code }

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
}

type imageEdit struct {
	part  map[string]any
	image *inlineImage
}

// Rewrite visits typed images in messages and function/custom tool results.
// All images validate before any upload; source changes only after every upload
// succeeds. An empty scope disables reuse across requests. Caller maps must not
// be accessed concurrently. Existing HTTPS references and file IDs are untouched.
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
			if u == nil || client == nil {
				return false, fail(503, "attachment_unavailable", "Basis Points attachment transport is unavailable")
			}
			if err := ctx.Err(); err != nil {
				return false, canceled(err)
			}
			if endpoint == "" {
				var err error
				endpoint, err = attachmentURL(responsesURL)
				if err != nil {
					return false, err
				}
			}
			path := fmt.Sprintf("input[%d].%s[%d]", i, field, j)
			invalid := func(message string) error { return fail(400, "invalid_image", message+" (path="+path+")") }
			if len(edits) >= maxRequestImages {
				return false, invalid("At most 20 inline images are allowed per request")
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
				img.key, err = u.imageKey(ctx, endpoint, scope, headers, img)
				if err != nil {
					return false, err
				}
				if existing := unique[img.key]; existing != nil {
					img = existing
				} else {
					// The keyed digest covers the entire payload, MIME and
					// identity. A hit can skip repeated base64/format scans.
					if scope == "" || u.cached(img.key) == "" {
						if err := validateImage(ctx, img); err != nil {
							if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
								return false, canceled(err)
							}
							return false, invalid(err.Error())
						}
					}
					unique[img.key] = img
					order = append(order, img)
				}
				// Request-local references only; payloads never enter the
				// persistent cache. Duplicate blocks avoid repeated HMAC work.
				if firstImage == nil {
					firstRaw, firstImage = raw, img
				} else {
					if encoded == nil {
						encoded = make(map[string]*inlineImage)
					}
					encoded[raw] = img
				}
			}
			total += img.size
			if total > maxRequestBytes {
				return false, invalid("Inline images exceed the 32 MiB request limit")
			}
			edits = append(edits, imageEdit{part: part, image: img})
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
		delete(edit.part, "image_url")
		edit.part["file_id"] = ids[edit.image.key]
		if _, exists := edit.part["detail"]; !exists {
			edit.part["detail"] = "auto"
		}
	}
	return true, nil
}

func (u *Uploader) cached(key [32]byte) string {
	u.mu.Lock()
	defer u.mu.Unlock()
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
	if id := u.cached(key); id != "" {
		return id, nil
	}
	u.mu.Lock()
	// Another upload can finish between cached() and this lock.
	if entry, ok := u.cache[key]; ok && u.now().Before(entry.expires) {
		u.mu.Unlock()
		return entry.fileID, nil
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
		for key, value := range u.cache {
			if !now.Before(value.expires) {
				delete(u.cache, key)
			}
		}
		if len(u.cache) >= maxCacheEntries {
			var oldest [32]byte
			oldestUsed := ^uint64(0)
			for key, value := range u.cache {
				if value.used < oldestUsed {
					oldest, oldestUsed = key, value.used
				}
			}
			delete(u.cache, oldest)
		}
		u.sequence++
		u.cache[key] = cacheEntry{fileID: id, expires: now.Add(cacheTTL), used: u.sequence}
	}
	pending.fileID, pending.err = id, err
	close(pending.done)
	u.mu.Unlock()
	return id, err
}
