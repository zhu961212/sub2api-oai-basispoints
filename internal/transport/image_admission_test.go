package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestImageAdmissionReservesUnknownLengthAndReleases(t *testing.T) {
	var budget imageRequestAdmission
	release, err := budget.acquire(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := budget.acquire(1); err == nil {
		t.Fatal("unknown length did not reserve full body budget")
	}
	release()
	var releases []func()
	for i := 0; i < 32; i++ {
		done, err := budget.acquire(100)
		if err != nil {
			t.Fatalf("small request %d: %v", i, err)
		}
		releases = append(releases, done)
	}
	if _, err := budget.acquire(100); err == nil {
		t.Fatal("more than 32 requests admitted")
	}
	for _, done := range releases {
		done()
	}
	if budget.requests != 0 || budget.bytes != 0 {
		t.Fatal("request budget leaked")
	}
	_, err = budget.acquire(maxImageRequestBytes + 1)
	var api *protocol.APIError
	if !errors.As(err, &api) || api.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body accepted: %v", err)
	}
}

func TestImageAdmissionRejectsBeforeUploadingImages(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	release, err := tr.imageAdmission.acquire(0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, image := relayTestImage(t)
	frames := requestFrames(t, "https://unused.example", token(t, "acct-budget"), nil, relayTestBody(image))
	stub := &streamStub{ctx: context.Background(), requests: frames}
	if err := tr.Forward(stub); err != nil {
		t.Fatal(err)
	}
	if stub.index != len(frames) {
		t.Fatal("image admission must follow typed body validation")
	}
	if len(stub.responses) != 3 || stub.responses[0].GetStart().GetStatusCode() != http.StatusServiceUnavailable || stub.responses[0].GetStart().GetStatus() != "503 Service Unavailable" || stub.responses[2].GetEnd() == nil {
		t.Fatalf("budget rejection lost HTTP status: %v", stub.responses)
	}
	var envelope map[string]map[string]any
	if err := json.Unmarshal(stub.responses[1].GetBodyChunk(), &envelope); err != nil || envelope["error"]["type"] != "server_error" || envelope["error"]["code"] != "image_request_busy" {
		t.Fatal("budget rejection lost server error metadata")
	}
	if got := stub.responses[0].GetStart().GetHeaders()["Retry-After"].GetValues(); len(got) != 1 || got[0] != "1" {
		t.Fatal("busy response omitted retry-after")
	}
}

func TestImageAdmissionReleasesOnImageValidationFailure(t *testing.T) {
	tr := New()
	defer tr.Shutdown()
	frames := requestFrames(t, "https://unused.example", token(t, "acct-validation"), nil, relayTestBody("data:image/png;base64,PRIVATE"))
	stub := &streamStub{ctx: context.Background(), requests: frames}
	if err := tr.Forward(stub); err != nil {
		t.Fatal(err)
	}
	assertRequestValidationHTTPError(t, stub.responses, http.StatusBadRequest, "invalid_image")
	if tr.imageAdmission.bytes != 0 || tr.imageAdmission.requests != 0 {
		t.Fatal("failed validation leaked request budget")
	}
}
