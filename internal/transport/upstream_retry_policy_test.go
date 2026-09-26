package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type retryPolicyRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip retryPolicyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type retryPolicyBody struct {
	io.Reader
	closed atomic.Int32
}

func (body *retryPolicyBody) Close() error {
	body.closed.Add(1)
	return nil
}

func retryPolicyRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/responses?fixture=retry", strings.NewReader("prepared request body"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer fixture-token")
	request.Header.Set("ChatGPT-Account-ID", "fixture-account")
	return request
}

func TestBasisPointsRetryExhaustionPreservesFinalResponseAndRequest(t *testing.T) {
	request := retryPolicyRequest(t, context.Background())
	request.Header.Set("X-Stainless-Retry-Count", "17")
	request.Header["X-Multiple"] = []string{"first", "second"}
	originalHeaders := request.Header.Clone()
	originalBody, originalURL, originalLength := request.Body, request.URL, request.ContentLength
	var responses []*http.Response
	var bodies []*retryPolicyBody
	client := &http.Client{Transport: retryPolicyRoundTripper(func(current *http.Request) (*http.Response, error) {
		attempt := len(responses) + 1
		if current.Method != request.Method || current.URL.String() != originalURL.String() || current.ContentLength != originalLength || current.Context() != request.Context() {
			t.Error("retry changed request method, URL, length, or context")
		}
		wantHeaders := originalHeaders.Clone()
		if attempt > 1 {
			wantHeaders.Set("X-Stainless-Retry-Count", fmt.Sprint(attempt-1))
			if current == request {
				t.Error("retry mutated the original request instead of cloning it")
			}
		}
		if !reflect.DeepEqual(current.Header, wantHeaders) {
			t.Errorf("attempt %d changed identity headers: got=%v want=%v", attempt, current.Header, wantHeaders)
		}
		raw, err := io.ReadAll(current.Body)
		_ = current.Body.Close()
		if err != nil || string(raw) != "prepared request body" {
			t.Errorf("attempt %d changed request bytes: body=%q err=%v", attempt, raw, err)
		}
		body := &retryPolicyBody{Reader: strings.NewReader(fmt.Sprintf("failure body %d", attempt))}
		response := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"0"}, "X-Request-Id": {fmt.Sprintf("attempt-%d", attempt)}}, Body: body}
		bodies = append(bodies, body)
		responses = append(responses, response)
		return response, nil
	})}
	response, err := doBasisPointsRequest(client, request)
	if err != nil || len(responses) != basisPointsHTTPAttempts || response != responses[len(responses)-1] || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("retry exhaustion lost final response: attempts=%d response=%v err=%v", len(responses), response, err)
	}
	for index, body := range bodies {
		wantClosed := int32(1)
		if index == len(bodies)-1 {
			wantClosed = 0
		}
		if body.closed.Load() != wantClosed {
			t.Errorf("response body %d close count=%d want=%d", index+1, body.closed.Load(), wantClosed)
		}
	}
	raw, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(raw) != "failure body 3" || response.Header.Get("X-Request-Id") != "attempt-3" {
		t.Fatalf("final failure content changed: body=%q err=%v", raw, err)
	}
	if !reflect.DeepEqual(request.Header, originalHeaders) || request.Body != originalBody || request.URL != originalURL || request.ContentLength != originalLength {
		t.Fatal("retry modified original request fields")
	}
}

func TestBasisPointsRetryExcludedStatuses(t *testing.T) {
	for _, status := range []int{400, 401, 403, 413, 422, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			body := &retryPolicyBody{Reader: strings.NewReader("unchanged upstream rejection")}
			want := &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"0"}}, Body: body}
			calls := 0
			client := &http.Client{Transport: retryPolicyRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				_ = request.Body.Close()
				return want, nil
			})}
			response, err := doBasisPointsRequest(client, retryPolicyRequest(t, context.Background()))
			if err != nil || calls != 1 || response != want || body.closed.Load() != 0 {
				t.Fatalf("excluded status retried or changed response: calls=%d response=%v err=%v", calls, response, err)
			}
			_ = response.Body.Close()
		})
	}
}

func TestBasisPointsRetryDelayBoundaries(t *testing.T) {
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name            string
		status, attempt int
		retryAfter      string
		delay           time.Duration
		retry           bool
	}{
		{"default", 500, 0, "", 250 * time.Millisecond, true},
		{"second_attempt", 502, 1, "", 500 * time.Millisecond, true},
		{"zero", 503, 0, "0", 250 * time.Millisecond, true},
		{"trimmed_seconds", 504, 0, " 2 ", 2 * time.Second, true},
		{"maximum_seconds", 503, 0, "5", 5 * time.Second, true},
		{"long_seconds", 503, 0, "6", 0, false},
		{"huge_seconds", 503, 0, "9223372036854775807", 0, false},
		{"negative_seconds", 503, 0, "-1", 250 * time.Millisecond, true},
		{"invalid", 503, 0, "not-a-delay", 250 * time.Millisecond, true},
		{"date", 503, 0, now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second, true},
		{"maximum_date", 503, 0, now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second, true},
		{"long_date", 503, 0, now.Add(6 * time.Second).Format(http.TimeFormat), 6 * time.Second, false},
		{"past_date", 503, 0, now.Add(-time.Hour).Format(http.TimeFormat), 250 * time.Millisecond, true},
		{"rate_limit", 429, 0, "0", 0, false},
	}
	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			response := &http.Response{StatusCode: fixture.status, Header: http.Header{"Retry-After": {fixture.retryAfter}}}
			delay, retry := basisPointsRetryDelay(response, fixture.attempt, now)
			if delay != fixture.delay || retry != fixture.retry {
				t.Fatalf("delay=%s retry=%t; want delay=%s retry=%t", delay, retry, fixture.delay, fixture.retry)
			}
		})
	}
}

func TestBasisPointsRetryPreservesResponseWhenRetryUnavailable(t *testing.T) {
	for _, reason := range []string{"long_delay", "deadline", "unreplayable", "body_error", "server_disallowed"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			if reason == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			request := retryPolicyRequest(t, ctx)
			if reason == "unreplayable" {
				request.GetBody = nil
			}
			if reason == "body_error" {
				request.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("body unavailable") }
			}
			body := &retryPolicyBody{Reader: strings.NewReader("available upstream diagnostic")}
			want := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: body}
			if reason == "long_delay" {
				want.Header.Set("Retry-After", "6")
			}
			if reason == "server_disallowed" {
				want.Header.Set("X-Should-Retry", "false")
			}
			calls := 0
			client := &http.Client{Transport: retryPolicyRoundTripper(func(current *http.Request) (*http.Response, error) {
				calls++
				_ = current.Body.Close()
				return want, nil
			})}
			response, err := doBasisPointsRequest(client, request)
			if err != nil || response != want || calls != 1 || body.closed.Load() != 0 {
				t.Fatalf("unavailable retry consumed final response: calls=%d response=%v err=%v", calls, response, err)
			}
			raw, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(raw) != "available upstream diagnostic" {
				t.Fatalf("upstream error body changed: raw=%q err=%v", raw, err)
			}
		})
	}
}

func TestBasisPointsRetryCancellationClosesPendingBodies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := retryPolicyRequest(t, ctx)
	pendingBody := &retryPolicyBody{Reader: strings.NewReader("prepared request body")}
	prepared := make(chan struct{})
	request.GetBody = func() (io.ReadCloser, error) {
		close(prepared)
		return pendingBody, nil
	}
	responseBody := &retryPolicyBody{Reader: strings.NewReader("temporary failure")}
	var calls atomic.Int32
	client := &http.Client{Transport: retryPolicyRoundTripper(func(current *http.Request) (*http.Response, error) {
		calls.Add(1)
		_ = current.Body.Close()
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"5"}}, Body: responseBody}, nil
	})}
	finished := make(chan error, 1)
	go func() {
		response, err := doBasisPointsRequest(client, request)
		if response != nil {
			finished <- errors.New("canceled backoff returned an already-closed response")
			return
		}
		finished <- err
	}()
	select {
	case <-prepared:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("request never entered retry backoff")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 || responseBody.closed.Load() != 1 || pendingBody.closed.Load() != 1 {
			t.Fatalf("cancellation leaked bodies or retried: err=%v calls=%d response_closed=%d pending_closed=%d", err, calls.Load(), responseBody.closed.Load(), pendingBody.closed.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not interrupt retry backoff")
	}
}

func TestBasisPointsRetryDoesNotReplayAmbiguousNetworkFailure(t *testing.T) {
	request := retryPolicyRequest(t, context.Background())
	wantErr := errors.New("connection reset after write")
	calls, clones := 0, 0
	request.GetBody = func() (io.ReadCloser, error) {
		clones++
		return io.NopCloser(strings.NewReader("prepared request body")), nil
	}
	client := &http.Client{Transport: retryPolicyRoundTripper(func(current *http.Request) (*http.Response, error) {
		calls++
		_ = current.Body.Close()
		return nil, wantErr
	})}
	response, err := doBasisPointsRequest(client, request)
	if response != nil || !errors.Is(err, wantErr) || calls != 1 || clones != 0 {
		t.Fatalf("network error retried: response=%v err=%v calls=%d clones=%d", response, err, calls, clones)
	}
}

type retryPolicyFailedRead struct{}

func (retryPolicyFailedRead) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestBasisPointsRetryNeverReplaysSuccessfulStream(t *testing.T) {
	for _, partialFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial_failure=%t", partialFailure), func(t *testing.T) {
			const data = "data: response.started"
			var reader io.Reader = strings.NewReader(data)
			if partialFailure {
				reader = io.MultiReader(reader, retryPolicyFailedRead{})
			}
			body := &retryPolicyBody{Reader: reader}
			want := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
			calls := 0
			client := &http.Client{Transport: retryPolicyRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				_ = request.Body.Close()
				return want, nil
			})}
			response, err := doBasisPointsRequest(client, retryPolicyRequest(t, context.Background()))
			if err != nil || response != want || calls != 1 || body.closed.Load() != 0 {
				t.Fatalf("successful stream was retried or consumed: calls=%d response=%v err=%v", calls, response, err)
			}
			raw, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if string(raw) != data || (partialFailure && !errors.Is(readErr, io.ErrUnexpectedEOF)) || (!partialFailure && readErr != nil) || calls != 1 {
				t.Fatalf("stream failure changed or replayed: body=%q err=%v calls=%d", raw, readErr, calls)
			}
		})
	}
}
