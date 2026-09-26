package transport

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
	"google.golang.org/grpc"
)

type degradationDirectoryHost struct {
	*degradationTestHost
	directory *pluginv1.ListAccountsResponse
}

func (h *degradationDirectoryHost) ListAccounts(context.Context, *pluginv1.ListAccountsRequest, ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	return h.directory, nil
}

func TestDegradationCheckDeduplicatesDirectoryAndHonorsUnavailableRows(t *testing.T) {
	for _, fixture := range []struct {
		name      string
		directory *pluginv1.ListAccountsResponse
		wantIDs   []int64
		wantState []string
	}{
		{
			name: "rich directory",
			directory: &pluginv1.ListAccountsResponse{
				Accounts: []*pluginv1.AccountInfo{
					{Id: 7, Name: "first", Schedulable: true},
					{Id: 7, Name: "duplicate", Schedulable: true},
					{Id: 9, Schedulable: true},
					{Id: 9, Schedulable: false},
					{Id: 11, Schedulable: false},
					{Id: 11, Schedulable: true},
				},
				AccountIds: []int64{7, 9, 11, 99},
			},
			wantIDs: []int64{7, 9, 11}, wantState: []string{"ok", "skipped", "skipped"},
		},
		{
			name:      "legacy directory",
			directory: &pluginv1.ListAccountsResponse{AccountIds: []int64{7, 7, 0, -1}},
			wantIDs:   []int64{7}, wantState: []string{"ok"},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			host := &degradationDirectoryHost{
				degradationTestHost: &degradationTestHost{fakeHost: &fakeHost{token: token(t, "account")}},
				directory:           fixture.directory,
			}
			tr := New()
			defer tr.Shutdown()
			tr.host = host
			tr.client = &http.Client{Transport: degradationRoundTripper(func(*http.Request) (*http.Response, error) {
				body := protocol.JSONBytes(map[string]any{"output_text": "iPhone 17"})
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}
			check, err := tr.runDegradationCheck(context.Background(), protocol.DefaultConfig())
			if err != nil || !check.Completed || len(check.Results) != len(fixture.wantIDs) {
				t.Fatalf("unexpected check: %+v err=%v", check, err)
			}
			for index, result := range check.Results {
				if result.AccountID != fixture.wantIDs[index] || result.Status != fixture.wantState[index] {
					t.Fatalf("result %d: %+v", index, result)
				}
			}
			if !reflect.DeepEqual(host.resolvedAccountIDs, []int64{7}) {
				t.Fatalf("duplicate or unavailable identity resolved: %v", host.resolvedAccountIDs)
			}
		})
	}
}

func TestDegradationCheckBoundsWorkersAndStopsQueuedIdentityResolution(t *testing.T) {
	host := &degradationTestHost{fakeHost: &fakeHost{token: token(t, "account")}}
	for accountID := int64(1); accountID <= 400; accountID++ {
		host.accounts = append(host.accounts, &pluginv1.AccountInfo{Id: accountID, Schedulable: true})
	}
	tr := New()
	defer tr.Shutdown()
	tr.host = host
	started := make(chan struct{}, degradationCheckParallel)
	tr.client = &http.Client{Transport: degradationRoundTripper(func(request *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseline := runtime.NumGoroutine()
	done := make(chan degradationCheckResult, 1)
	go func() {
		check, _ := tr.runDegradationCheckWithBudget(ctx, protocol.DefaultConfig(), time.Second)
		done <- check
	}()
	for worker := 0; worker < degradationCheckParallel; worker++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	if added := runtime.NumGoroutine() - baseline; added > degradationCheckParallel+16 {
		t.Errorf("large directory created %d goroutines for %d workers", added, degradationCheckParallel)
	}
	cancel()
	select {
	case check := <-done:
		if check.Completed || len(check.Results) != 400 {
			t.Fatalf("canceled results incomplete: %+v", check)
		}
		for _, result := range check.Results {
			if result.Status != "error" || result.Error == "" {
				t.Fatalf("canceled queued account has no verdict: %+v", result)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("workers did not stop after cancellation")
	}
	if len(host.resolvedAccountIDs) != degradationCheckParallel {
		t.Fatalf("queued identities resolved after cancellation: %d", len(host.resolvedAccountIDs))
	}
}

func TestCanceledDegradationCheckDoesNotResolveIdentity(t *testing.T) {
	host := &degradationTestHost{fakeHost: &fakeHost{token: token(t, "account")}}
	tr := New()
	defer tr.Shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, answer, err := tr.checkDegradationAccount(ctx, protocol.DefaultConfig(), host, tr.client, 7, protocol.DefaultModelID)
	if status != "error" || answer != "" || err == nil || len(host.resolvedAccountIDs) != 0 {
		t.Fatalf("canceled check resolved identity: status=%s answer=%q err=%v IDs=%v", status, answer, err, host.resolvedAccountIDs)
	}
}

func TestDegradationCheckDoesNotReplaceInvalidMetadataWithLegacyIDs(t *testing.T) {
	host := &degradationDirectoryHost{
		degradationTestHost: &degradationTestHost{fakeHost: &fakeHost{token: token(t, "account")}},
		directory: &pluginv1.ListAccountsResponse{
			Accounts:   []*pluginv1.AccountInfo{nil, {Id: -7, Schedulable: true}},
			AccountIds: []int64{7},
		},
	}
	tr := New()
	defer tr.Shutdown()
	tr.host = host
	check, err := tr.runDegradationCheck(context.Background(), protocol.DefaultConfig())
	if err == nil || check.Completed || len(check.Results) != 0 || len(host.resolvedAccountIDs) != 0 {
		t.Fatalf("invalid metadata fell back to credential-bearing IDs: check=%+v err=%v IDs=%v", check, err, host.resolvedAccountIDs)
	}
}
