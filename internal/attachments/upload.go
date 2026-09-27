package attachments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

func attachmentURL(responsesURL string) (string, error) {
	u, err := url.Parse(strings.TrimRight(responsesURL, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.TrimSpace(responsesURL) != responsesURL {
		return "", fail(500, "attachment_config", "Cannot derive the native attachment endpoint from responses_url")
	}
	return u.ResolveReference(&url.URL{Path: "attachments"}).String(), nil
}

func (u *Uploader) upload(ctx context.Context, client *http.Client, endpoint string, headers http.Header, img *inlineImage) (string, error) {
	return u.uploadAttachment(ctx, client, endpoint, headers, "image."+strings.TrimPrefix(img.mime, "image/"), img.mime, img.payload, img.size)
}

func (u *Uploader) uploadAttachment(ctx context.Context, client *http.Client, endpoint string, headers http.Header, filename, mime, payload string, size int64) (string, error) {
	select {
	case u.uploads <- struct{}{}:
		defer func() { <-u.uploads }()
	case <-ctx.Done():
		return "", canceled(ctx.Err())
	default:
		return "", fail(503, "attachment_busy", "Basis Points attachment uploads are busy; retry shortly")
	}
	var framing bytes.Buffer
	writer := multipart.NewWriter(&framing)
	partHeaders := make(textproto.MIMEHeader)
	// MIME quoted strings only escape quote/backslash. Go %q additionally
	// emits \uXXXX for valid Unicode filename characters such as NBSP; MIME
	// readers do not decode those escapes and would change the filename.
	quotedName := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(filename)
	partHeaders.Set("Content-Disposition", "form-data; name=\"file\"; filename=\""+quotedName+"\"")
	partHeaders.Set("Content-Type", mime)
	if _, err := writer.CreatePart(partHeaders); err != nil {
		return "", fail(500, "attachment_encoding", "Cannot encode attachment")
	}
	separator := framing.Len()
	if writer.Close() != nil {
		return "", fail(500, "attachment_encoding", "Cannot finish attachment")
	}
	prefix, suffix := framing.Bytes()[:separator], framing.Bytes()[separator:]
	// Only small multipart framing is buffered. Decode directly into the HTTP
	// transport without an image-sized copy, disk file, or writer goroutine.
	body := io.MultiReader(bytes.NewReader(prefix), base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(payload)), bytes.NewReader(suffix))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", fail(500, "attachment_config", "Cannot construct native attachment request")
	}
	request.ContentLength = int64(len(prefix)+len(suffix)) + size
	request.Header = headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Del("Content-Length")
	request.Header.Del("Transfer-Encoding")
	request.Header.Del("Content-Encoding")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	// Reuse the transport/proxy and timeouts, but never follow redirects.
	// Thus bearer tokens and account headers cannot cross upstream origins.
	isolated := *client
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := isolated.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", canceled(ctx.Err())
		}
		return "", fail(502, "attachment_transport", "Basis Points attachment upload transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		if status < 400 {
			status = 502
		}
		return "", fail(status, "attachment_upload_error", fmt.Sprintf("Basis Points attachment upload returned HTTP %d", response.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", canceled(ctx.Err())
		}
		return "", fail(502, "invalid_attachment_response", "Basis Points attachment upload returned an invalid response")
	}
	if len(raw) > maxResponseBytes {
		return "", fail(502, "invalid_attachment_response", "Basis Points attachment upload returned an invalid response")
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil {
		return "", fail(502, "invalid_attachment_response", "Basis Points attachment upload returned no valid openai_file_id")
	}
	id, _ := result["openai_file_id"].(string)
	if !validFileID(id) {
		return "", fail(502, "invalid_attachment_response", "Basis Points attachment upload returned no valid openai_file_id")
	}
	return id, nil
}

func validFileID(id string) bool {
	if !strings.HasPrefix(id, "file-") || len(id) < 6 || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
