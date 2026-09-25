package attachments

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestCanceledSharedOwnerDoesNotCancelActiveWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		u := New()
		ownerCtx, cancelOwner := context.WithCancel(context.Background())
		defer cancelOwner()
		waiterCtx, cancelWaiter := context.WithCancel(context.Background())
		defer cancelWaiter()
		var key [32]byte
		started := make(chan struct{})
		ownerDone := make(chan error, 1)
		type result struct {
			id  string
			err error
		}
		waiterDone := make(chan result, 1)
		var calls atomic.Int32
		go func() {
			_, err := u.getOrUpload(ownerCtx, key, true, func() (string, error) {
				calls.Add(1)
				close(started)
				<-ownerCtx.Done()
				return "", canceled(ownerCtx.Err())
			})
			ownerDone <- err
		}()
		<-started
		go func() {
			id, err := u.getOrUpload(waiterCtx, key, true, func() (string, error) {
				calls.Add(1)
				request, err := http.NewRequestWithContext(waiterCtx, http.MethodPost, "https://example.invalid/attachments", nil)
				if err != nil {
					return "", err
				}
				if request.Context() != waiterCtx || request.Context().Err() != nil {
					return "", errors.New("waiter inherited canceled owner context")
				}
				return "file-waiter", nil
			})
			waiterDone <- result{id, err}
		}()
		// Both goroutines are durably blocked: the owner on its context and
		// the waiter on the owner's pending upload. No wall-clock sleep/race.
		synctest.Wait()
		cancelOwner()
		if err := <-ownerDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("owner cancellation lost: %v", err)
		}
		got := <-waiterDone
		if got.err != nil || got.id != "file-waiter" || waiterCtx.Err() != nil || calls.Load() != 2 {
			t.Fatalf("active waiter failed after owner canceled: %#v, calls=%d", got, calls.Load())
		}
		if id := u.cached(key); id != "file-waiter" {
			t.Fatalf("successful replacement was not cached: %q", id)
		}
	})
}

func TestSharedOrdinaryUploadFailureIsNotRetried(t *testing.T) {
	for _, code := range []int{400, 401, 429, 502, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				u := New()
				var key [32]byte
				started, finish := make(chan struct{}), make(chan struct{})
				errs := make(chan error, 2)
				want := fail(code, "attachment_upload_error", "safe fixture error")
				var calls atomic.Int32
				go func() {
					_, err := u.getOrUpload(context.Background(), key, true, func() (string, error) {
						calls.Add(1)
						close(started)
						<-finish
						return "", want
					})
					errs <- err
				}()
				<-started
				go func() {
					_, err := u.getOrUpload(context.Background(), key, true, func() (string, error) {
						calls.Add(1)
						return "file-should-not-upload", nil
					})
					errs <- err
				}()
				synctest.Wait()
				close(finish)
				for range 2 {
					if err := <-errs; err != want {
						t.Fatalf("ordinary failure was replaced: %v", err)
					}
				}
				if calls.Load() != 1 {
					t.Fatalf("ordinary upload failure retried %d times", calls.Load()-1)
				}
			})
		})
	}
}

func TestCanceledOwnerTakeoverDoesNotRetryItsOwnFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		u := New()
		var key [32]byte
		started, finish := make(chan struct{}), make(chan struct{})
		errs := make(chan error, 2)
		var calls atomic.Int32
		upload := func() (string, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-finish
			}
			return "", canceled(context.DeadlineExceeded)
		}
		go func() { _, err := u.getOrUpload(context.Background(), key, true, upload); errs <- err }()
		<-started
		go func() { _, err := u.getOrUpload(context.Background(), key, true, upload); errs <- err }()
		synctest.Wait()
		close(finish)
		for range 2 {
			if err := <-errs; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation error changed: %v", err)
			}
		}
		if calls.Load() != 2 {
			t.Fatalf("takeover retried beyond once: %d uploads", calls.Load())
		}
	})
}
