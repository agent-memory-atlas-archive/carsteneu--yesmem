package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLos11iBearerAuthFuncRequiresBearerPrefix is the regression test for
// YM-345 (2): BearerAuthFunc looked only at byte offset 7, so any 7 characters
// in front of the token authenticated. The scheme is part of the credential
// format, not decoration.
func TestLos11iBearerAuthFuncRequiresBearerPrefix(t *testing.T) {
	handler := BearerAuthFunc("mytoken", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name string
		auth string
		want int
		why  string
	}{
		{"correct scheme", "Bearer mytoken", http.StatusOK, "the documented form must pass"},
		{"foreign prefix", "Xxxxxxxmytoken", http.StatusUnauthorized, "a 7-byte prefix of arbitrary text is not a scheme"},
		{"lowercase scheme", "bearer mytoken", http.StatusOK, "the scheme name is case-insensitive (RFC 7235 2.1)"},
		{"scheme only", "Bearer ", http.StatusUnauthorized, "empty token must not authenticate"},
		{"scheme without separator", "Bearermytoken", http.StatusUnauthorized, "the separator is part of the format"},
		{"no header", "", http.StatusUnauthorized, "missing header must not authenticate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/test", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			handler(w, req)
			if w.Code != tc.want {
				t.Errorf("Authorization %q: got %d, want %d (%s)", tc.auth, w.Code, tc.want, tc.why)
			}
		})
	}
}

// TestLos11iBearerAuthFuncEmptyTokenRejectsEverything pins the fail-closed
// guard for an empty configured token: with "" as the token, "Bearer " has a
// valid prefix and an empty remainder, which would otherwise authenticate.
func TestLos11iBearerAuthFuncEmptyTokenRejectsEverything(t *testing.T) {
	handler := BearerAuthFunc("", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, auth := range []string{"Bearer ", "Bearer x", ""} {
		req := httptest.NewRequest("GET", "/api/test", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		handler(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("empty configured token, Authorization %q: got %d, want 401", auth, w.Code)
		}
	}
}
