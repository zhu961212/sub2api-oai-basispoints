package transport

import (
	"net/http"
	"testing"

	"github.com/wangyunjeff/sub2api-oai-basispoints/internal/protocol"
)

func TestAccountWithoutProxyDoesNotInheritEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://environment-proxy.invalid:8080")
	client := newClient(protocol.DefaultConfig())
	defer client.CloseIdleConnections()
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("empty account proxy must mean direct egress")
	}
}

func TestNonStreamInvalidToolIsNotReturnedAsRawNativeCall(t *testing.T) {
	_, _, err := transformResponse([]byte(`{"status":"completed","output":[{"type":"function_call","name":"run_officejs","arguments":"{}"}]}`), http.Header{"Content-Type": []string{"application/json"}}, map[string]any{})
	if err == nil {
		t.Fatal("invalid native tool escaped validation")
	}
}
