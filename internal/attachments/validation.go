package attachments

import (
	"context"
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
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	_ "golang.org/x/image/webp"
)

func parseImage(raw string) (*inlineImage, error) {
	img, err := parseImageMetadata(raw)
	if err == nil {
		err = validateBase64Shape(img.payload)
	}
	return img, err
}

// Metadata checks are constant-time in payload size. The full encoded payload
// is authenticated before cache lookup; a hit is already completely validated.
func parseImageMetadata(raw string) (*inlineImage, error) {
	if len(raw) < 5 || !strings.EqualFold(raw[:5], "data:") {
		return nil, errors.New("Inline image requires a base64 image data URL")
	}
	comma := strings.IndexByte(raw[5:min(len(raw), 38)], ',')
	if comma < 0 {
		return nil, errors.New("Inline image requires a base64 image data URL")
	}
	header, payload := raw[5:5+comma], raw[6+comma:]
	mime := ""
	switch strings.ToLower(header) {
	case "image/png;base64":
		mime = "image/png"
	case "image/jpeg;base64":
		mime = "image/jpeg"
	case "image/gif;base64":
		mime = "image/gif"
	case "image/webp;base64":
		mime = "image/webp"
	default:
		return nil, errors.New("Inline images require PNG, JPEG, GIF or WebP with base64 encoding")
	}
	if len(payload) > base64.StdEncoding.EncodedLen(maxImageBytes) {
		return nil, errors.New("Inline image exceeds the 20 MiB limit")
	}
	if len(payload) < 4 || len(payload)%4 != 0 {
		return nil, errors.New("Inline image contains invalid base64 data")
	}
	var last [3]byte
	tail, err := base64.StdEncoding.Strict().Decode(last[:], []byte(payload[len(payload)-4:]))
	if err != nil {
		return nil, errors.New("Inline image contains invalid base64 data")
	}
	size := int64(len(payload)/4-1)*3 + int64(tail)
	if size == 0 || size > maxImageBytes {
		return nil, errors.New("Inline image is empty or exceeds the 20 MiB limit")
	}
	return &inlineImage{mime: mime, payload: payload, size: size}, nil
}

// A streaming base64 decoder can otherwise accept concatenated padded segments.
func validateBase64Shape(payload string) error {
	if strings.ContainsAny(payload, " \r\n\t") || strings.IndexByte(payload[:len(payload)-2], '=') >= 0 {
		return errors.New("Inline image contains invalid base64 data")
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

var scratch = sync.Pool{New: func() any { return new([32 << 10]byte) }}

func release(buffer *[32 << 10]byte) { clear(buffer[:]); scratch.Put(buffer) }

func validateImage(ctx context.Context, img *inlineImage) error {
	if err := validateBase64Shape(img.payload); err != nil {
		return err
	}
	buffer := scratch.Get().(*[32 << 10]byte)
	defer release(buffer)
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(img.payload))
	n, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, contextReader{ctx, decoder}, buffer[:])
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Inline image contains invalid base64 data")
	}
	if n != img.size {
		return errors.New("Inline image contains invalid base64 data")
	}
	decoder = base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(img.payload))
	dimensions, format, err := image.DecodeConfig(contextReader{ctx, decoder})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || dimensions.Width <= 0 || dimensions.Height <= 0 || int64(dimensions.Width) > maxPixels/int64(dimensions.Height) {
		return errors.New("Inline image is invalid or exceeds 64 megapixels")
	}
	if img.mime != "image/"+format {
		return errors.New("Inline image MIME type does not match its contents")
	}
	return nil
}

func (u *Uploader) imageKey(ctx context.Context, endpoint, scope string, headers http.Header, img *inlineImage) ([32]byte, error) {
	mac := hmac.New(sha256.New, u.key[:])
	buffer := scratch.Get().(*[32 << 10]byte)
	defer release(buffer)
	var size [8]byte
	values := []string{endpoint, scope}
	// Header.Get alone misses deliberately noncanonical map keys. Digest all
	// case-insensitive credential variants, including duplicates, without storing
	// their values in the cache. Do not let two account identities alias.
	for _, name := range []string{"Authorization", "ChatGPT-Account-ID", "X-OpenAI-Account-ID", "X-Basispoints-Auth-Mode", "Cookie"} {
		var keys []string
		for key := range headers {
			if strings.EqualFold(key, name) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		values = append(values, name, strconv.Itoa(len(keys)))
		for _, key := range keys {
			values = append(values, key, strconv.Itoa(len(headers[key])))
			values = append(values, headers[key]...)
		}
	}
	values = append(values, img.mime, img.payload)
	for _, value := range values {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = mac.Write(size[:])
		for len(value) != 0 {
			if err := ctx.Err(); err != nil {
				return [32]byte{}, canceled(err)
			}
			n := copy(buffer[:], value)
			_, _ = mac.Write(buffer[:n])
			value = value[n:]
		}
	}
	var key [32]byte
	copy(key[:], mac.Sum(nil))
	return key, nil
}
