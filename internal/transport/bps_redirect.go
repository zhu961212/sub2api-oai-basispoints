package transport

import "net/http"

// BPS requests carry account/device identity and may contain conversation data.
// Even when net/http drops Authorization on a cross-origin redirect, it can
// still forward account headers and replay a 307/308 body. Share the existing
// transport, proxy and timeout, but never follow a BPS redirect.
func withoutBPSRedirects(client *http.Client) *http.Client {
	if client == nil {
		return nil
	}
	isolated := *client
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &isolated
}
