package imagerelay

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
)

var inlineBuffers = sync.Pool{New: func() any { return new([32 << 10]byte) }}

func releaseInlineBuffer(buffer *[32 << 10]byte) {
	clear(buffer[:])
	inlineBuffers.Put(buffer)
}

func inlinePayload(raw string) (contentType, payload string, err error) {
	header, payload, ok := strings.Cut(raw[5:], ",")
	if !ok {
		return "", "", invalid("inline image requires a base64 image data URL")
	}
	switch strings.ToLower(header) {
	case "image/png;base64":
		contentType = "image/png"
	case "image/jpeg;base64":
		contentType = "image/jpeg"
	case "image/gif;base64":
		contentType = "image/gif"
	case "image/webp;base64":
		contentType = "image/webp"
	default:
		return "", "", invalid("inline images require PNG, JPEG, GIF or WebP with base64 encoding")
	}
	if len(payload) == 0 || strings.ContainsAny(payload, " \r\n\t") {
		return "", "", invalid("inline image contains invalid base64 data")
	}
	if len(payload) > base64.StdEncoding.EncodedLen(maxImageBytes) {
		return "", "", invalid("inline image exceeds the 20 MiB limit")
	}
	return contentType, payload, nil
}

func (r *Relay) stageImage(raw, scope string) (*storedImage, string, error) {
	contentType, payload, err := inlinePayload(raw)
	if err != nil {
		return nil, "", err
	}
	n, err := inlineDecodedSize(payload)
	if err != nil {
		return nil, "", err
	}
	// Key the complete canonical encoding, not a decoded prefix. A cache hit
	// reuses content already strictly decoded and validated by its writer.
	// Scope length prevents one tenant's prefix from aliasing another's.
	token := r.inlineToken(payload, scope)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, "", ErrUnavailable
	}
	// The index includes pending writes and expired files held by readers or
	// rewrites. A pin protects reuse without renewing TTL before commit.
	if existing := r.images[token]; existing != nil {
		if existing.contentType != contentType {
			r.mu.Unlock()
			return nil, "", invalid("inline image MIME type does not match its contents")
		}
		existing.pins++
		r.mu.Unlock()
		<-existing.ready
		if existing.stageErr != nil {
			r.releaseImage(existing)
			return nil, "", existing.stageErr
		}
		return existing, token, nil
	}
	r.pruneLocked(time.Now())
	if r.usedBytes+n > maxStorageBytes || r.usedImages >= maxStorageImages {
		r.mu.Unlock()
		// Preserve malformed-base64 errors even when storage is full. No
		// file is reserved, and a valid replay was handled before this branch.
		if _, err := copyInline(io.Discard, payload); err != nil {
			return nil, "", err
		}
		return nil, "", ErrFull
	}
	r.usedBytes += n
	r.usedImages++
	img := &storedImage{token: token, contentType: contentType, size: n, charge: n, pins: 1, ready: make(chan struct{})}
	r.images[token] = img
	r.mu.Unlock()
	// Another request may pin this image, but cannot use it until writing and
	// DecodeConfig finish. No request waits on another request's full commit.
	err = r.writeImage(img, payload)
	r.mu.Lock()
	img.stageErr = err
	close(img.ready)
	r.mu.Unlock()
	if err != nil {
		r.releaseImage(img)
		return nil, "", err
	}
	return img, token, nil
}

// Exact encoded length and canonical final padding are cheap to check without
// decoding the image. Reject interior padding globally: streaming decoders can
// otherwise accept a new padded segment at an internal read boundary.
func inlineDecodedSize(payload string) (int64, error) {
	if len(payload) < 4 || len(payload)%4 != 0 || strings.IndexByte(payload[:len(payload)-2], '=') >= 0 {
		return 0, invalid("inline image contains invalid base64 data")
	}
	var last [3]byte
	tail, err := base64.StdEncoding.Strict().Decode(last[:], []byte(payload[len(payload)-4:]))
	if err != nil {
		return 0, invalid("inline image contains invalid base64 data")
	}
	n := int64(len(payload)/4-1)*3 + int64(tail)
	if n == 0 || n > maxImageBytes {
		return 0, invalid("inline image is empty or exceeds the 20 MiB limit")
	}
	return n, nil
}

func (r *Relay) inlineToken(payload, scope string) string {
	mac := hmac.New(sha256.New, r.key[:])
	var scopeLength [8]byte
	binary.BigEndian.PutUint64(scopeLength[:], uint64(len(scope)))
	_, _ = mac.Write(scopeLength[:])
	buffer := inlineBuffers.Get().(*[32 << 10]byte)
	defer releaseInlineBuffer(buffer)
	for _, value := range []string{scope, payload} {
		for len(value) > 0 {
			n := copy(buffer[:], value)
			_, _ = mac.Write(buffer[:n])
			value = value[n:]
		}
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func copyInline(dst io.Writer, payload string) (int64, error) {
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(payload))
	buffer := inlineBuffers.Get().(*[32 << 10]byte)
	defer releaseInlineBuffer(buffer)
	// Hide ReaderFrom so CopyBuffer actually uses the bounded scratch buffer.
	n, err := io.CopyBuffer(struct{ io.Writer }{dst}, io.LimitReader(decoder, maxImageBytes+1), buffer[:])
	if err != nil {
		var corrupt base64.CorruptInputError
		if errors.As(err, &corrupt) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, invalid("inline image contains invalid base64 data")
		}
		return 0, ErrUnavailable
	}
	if n == 0 || n > maxImageBytes {
		return 0, invalid("inline image is empty or exceeds the 20 MiB limit")
	}
	return n, nil
}

func (r *Relay) writeImage(img *storedImage, payload string) error {
	var file *os.File
	defer func() {
		if file != nil {
			_ = file.Close()
		}
	}()
	var err error
	file, err = os.CreateTemp(r.dir, "image-")
	if err != nil {
		return ErrUnavailable
	}
	img.path = file.Name()
	if file.Chmod(0600) != nil {
		return ErrUnavailable
	}
	// A base64 decoder produces small chunks. Buffer them before writing to
	// disk, and hide File.ReadFrom so it cannot bypass the buffer.
	writer := bufio.NewWriterSize(struct{ io.Writer }{file}, 64<<10)
	n, err := copyInline(writer, payload)
	if err != nil {
		return err
	}
	if n != img.size {
		return invalid("inline image contains invalid base64 data")
	}
	if err := writer.Flush(); err != nil {
		return ErrUnavailable
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ErrUnavailable
	}
	dimensions, format, err := image.DecodeConfig(file)
	if err != nil || dimensions.Width <= 0 || dimensions.Height <= 0 || int64(dimensions.Width) > maxPixels/int64(dimensions.Height) {
		return invalid("inline image is invalid or exceeds 64 megapixels")
	}
	if img.contentType != "image/"+format {
		return invalid("inline image MIME type does not match its contents")
	}
	if file.Close() != nil {
		file = nil
		return ErrUnavailable
	}
	file = nil
	return nil
}

func (r *Relay) releaseImage(img *storedImage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	img.pins--
	if r.entries[img.token] != img || !time.Now().Before(img.expires) {
		if r.entries[img.token] == img {
			delete(r.entries, img.token)
		}
		r.removeLocked(img)
	}
}

// Failed removals stay charged and are retried by the cleaner.
func (r *Relay) removeLocked(img *storedImage) {
	if img.readers != 0 || img.pins != 0 {
		r.retired[img] = true
		return
	}
	if img.path != "" {
		if err := os.Remove(img.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.retired[img] = true
			return
		}
	}
	r.usedBytes -= img.charge
	r.usedImages--
	delete(r.images, img.token)
	delete(r.retired, img)
}

func (r *Relay) pruneLocked(now time.Time) {
	for token, img := range r.entries {
		if !now.Before(img.expires) {
			delete(r.entries, token)
			r.removeLocked(img)
		}
	}
	for img := range r.retired {
		if img.readers == 0 && img.pins == 0 {
			r.removeLocked(img)
		}
	}
}

func (r *Relay) cleanupLoop() {
	defer close(r.cleanerDone)
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-ticker.C:
			r.mu.Lock()
			r.pruneLocked(now)
			r.mu.Unlock()
			r.cleanupOrphans(now)
		}
	}
}

// Timestamps alone cannot establish a crashed process. Every live session
// holds an OS file lock; reclamation requires acquiring that lock. New relays
// always create fresh directories and never adopt an existing session.
func (r *Relay) cleanupOrphans(now time.Time) {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") {
			continue
		}
		path := filepath.Join(r.root, entry.Name())
		if path == r.dir {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || now.Sub(info.ModTime()) <= imageTTL+cleanupInterval {
			continue
		}
		leasePath := filepath.Join(path, leaseName)
		leaseInfo, err := os.Lstat(leasePath)
		if err != nil || !leaseInfo.Mode().IsRegular() {
			continue
		}
		lease, err := os.OpenFile(leasePath, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if lockLease(lease) != nil {
			_ = lease.Close()
			continue
		}
		info, err = os.Lstat(path)
		stale := err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && now.Sub(info.ModTime()) > imageTTL+cleanupInterval
		_ = unlockLease(lease)
		_ = lease.Close()
		if stale {
			_ = os.RemoveAll(path)
		}
	}
}
