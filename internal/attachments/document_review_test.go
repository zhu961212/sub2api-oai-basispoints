package attachments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestMixedDocumentHistoryPreservesOccurrenceBudgetAndUploadReuse(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-reviewed"})
	}))
	defer server.Close()
	u := New()
	imgRaw := dataURL(testPNG(t), "image/png")
	fileRaw := dataURL(documentFixture(t, "sample.docx"), docxMIME)
	for turn := 1; turn <= maxRequestFiles+1; turn++ {
		source := message(imagePart(imgRaw))
		for history := 0; history < turn; history++ {
			source["input"] = append(source["input"].([]any), toolImageInput("custom_tool_call_output", filePart(fileRaw, "客户\u00a0合同.docx")))
		}
		err := ValidateMixedInputs(context.Background(), source)
		if turn > maxRequestFiles {
			var typed *Error
			if !errors.As(err, &typed) || typed.Code() != "invalid_file" {
				t.Fatalf("expanded history bypassed file count: %v", err)
			}
			if uploads.Load() != 2 {
				t.Fatalf("invalid history caused upload: %d", uploads.Load())
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = u.Rewrite(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "review-scope"); err != nil {
			t.Fatal(err)
		}
		if _, err = u.RewriteFiles(context.Background(), server.Client(), server.URL+"/responses", testHeaders(), source, "review-scope"); err != nil {
			t.Fatal(err)
		}
		if uploads.Load() != 2 {
			t.Fatalf("expanded history reuploaded cached attachments: turn=%d uploads=%d", turn, uploads.Load())
		}
	}
}

func TestDocumentCancellationDuringUploadDoesNotPublishCacheOrMutate(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			t.Error("upload connection was not canceled")
		}
		close(finished)
	}))
	defer server.Close()
	u := New()
	part := filePart(dataURL(documentFixture(t, "sample.pdf"), pdfMIME), "合同.pdf")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := u.RewriteFiles(ctx, server.Client(), server.URL+"/responses", testHeaders(), message(part), "review-scope")
		result <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("upload did not reach the test server")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
	<-finished
	if part["file_data"] == nil || part["file_id"] != nil || len(u.cache) != 0 || len(u.pending) != 0 {
		t.Fatal("canceled upload retained IDs or mutated the original attachment")
	}
}
