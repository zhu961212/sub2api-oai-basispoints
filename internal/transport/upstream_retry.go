package transport

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const basisPointsHTTPAttempts = 3

// Only retry explicit temporary HTTP failures before exposing a response.
// Successful streams and ambiguous network failures must never replay a turn.
func doBasisPointsRequest(client *http.Client, request *http.Request) (*http.Response, error) {
	current := request
	for attempt := 0; ; attempt++ {
		response, err := client.Do(current)
		if err != nil || attempt+1 >= basisPointsHTTPAttempts || request.GetBody == nil {
			return response, err
		}
		delay, retry := basisPointsRetryDelay(response, attempt, time.Now())
		if !retry {
			return response, nil
		}
		if deadline, ok := request.Context().Deadline(); ok && time.Until(deadline) <= delay {
			return response, nil
		}
		body, err := request.GetBody()
		if err != nil {
			return response, nil
		}
		_ = response.Body.Close()
		timer := time.NewTimer(delay)
		select {
		case <-request.Context().Done():
			timer.Stop()
			_ = body.Close()
			return nil, request.Context().Err()
		case <-timer.C:
		}
		if err := request.Context().Err(); err != nil {
			_ = body.Close()
			return nil, err
		}
		current = request.Clone(request.Context())
		current.Body = body
		current.Header.Set("X-Stainless-Retry-Count", strconv.Itoa(attempt+1))
	}
}

func basisPointsRetryDelay(response *http.Response, attempt int, now time.Time) (time.Duration, bool) {
	if strings.EqualFold(strings.TrimSpace(response.Header.Get("X-Should-Retry")), "false") {
		return 0, false
	}
	switch response.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	default:
		return 0, false
	}
	delay := 250 * time.Millisecond * time.Duration(1<<attempt)
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if value == "" {
		return delay, true
	}
	// Honor the server's minimum wait. Return long delays to the caller instead
	// of keeping an interactive request queued or retrying earlier than asked.
	seconds, err := strconv.ParseInt(value, 10, 64)
	// A positive delay can be valid decimal text yet exceed int64. Treat it
	// as a long wait instead of retrying early with the default backoff.
	if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(value, "-") {
		return 0, false
	}
	if err == nil {
		if seconds < 0 {
			return delay, true
		}
		if seconds > 5 {
			return 0, false
		}
		return max(delay, time.Duration(seconds)*time.Second), true
	}
	if until, err := http.ParseTime(value); err == nil {
		delay = max(delay, until.Sub(now))
		return delay, delay <= 5*time.Second
	}
	return delay, true
}
