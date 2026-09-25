package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func encodeSegment(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func fakeToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	return encodeSegment(t, map[string]any{"alg": "none"}) + "." + encodeSegment(t, claims) + "."
}

func TestParseTokenPrefersHostAccountID(t *testing.T) {
	token := fakeToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-from-jwt"},
	})
	identity := ParseToken("Bearer "+token, "acct-from-host")
	if identity.AccountID != "acct-from-host" {
		t.Fatalf("account id = %q, want acct-from-host", identity.AccountID)
	}
	if identity.AccessToken != token {
		t.Fatalf("access token was not normalized: %q", identity.AccessToken)
	}
}

func TestParseTokenFallsBackToJWTAccountID(t *testing.T) {
	token := fakeToken(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-from-jwt"},
	})
	identity := ParseToken(token, "")
	if identity.AccountID != "acct-from-jwt" {
		t.Fatalf("account id = %q, want acct-from-jwt", identity.AccountID)
	}
}

func TestParseTokenKeepsOpaqueTokenWithHostAccountID(t *testing.T) {
	identity := ParseToken("opaque-access-token", "acct-from-host")
	if identity.AccountID != "acct-from-host" {
		t.Fatalf("account id = %q, want acct-from-host", identity.AccountID)
	}
	if identity.AccessToken != "opaque-access-token" {
		t.Fatalf("access token = %q", identity.AccessToken)
	}
}

func TestParseTokenReadsEmailAndExpiry(t *testing.T) {
	token := fakeToken(t, map[string]any{
		"email":                       "person@example.test",
		"exp":                         4102444800,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct"},
	})
	identity := ParseToken(token, "")
	if identity.Email != "person@example.test" {
		t.Fatalf("email = %q", identity.Email)
	}
	if want := time.Unix(4102444800, 0); !identity.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %v, want %v", identity.ExpiresAt, want)
	}
	if identity.Expired(time.Unix(4102444799, 0)) {
		t.Fatal("token reported as expired before exp")
	}
	if !identity.Expired(time.Unix(4102444801, 0)) {
		t.Fatal("token reported as valid after exp")
	}
}

func TestExpiredTreatsMissingExpiryAsValid(t *testing.T) {
	identity := Identity{AccessToken: "opaque"}
	if identity.Expired(time.Now()) {
		t.Fatal("identity without exp must not be treated as expired")
	}
}

func TestStripBearerIsCaseInsensitive(t *testing.T) {
	for _, input := range []string{"Bearer abc", "bearer abc", "BEARER abc", "  Bearer abc  "} {
		if got := StripBearer(input); got != "abc" {
			t.Fatalf("StripBearer(%q) = %q, want abc", input, got)
		}
	}
	if got := StripBearer("abc"); got != "abc" {
		t.Fatalf("StripBearer preserved bare token incorrectly: %q", got)
	}
}
