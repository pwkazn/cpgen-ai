package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/config"
)

func TestLocalWorkbenchRejectsForeignHostOriginAndMissingCSRF(t *testing.T) {
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.ToSlash(filepath.Join(t.TempDir(), "state")) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := s.Handler()
	sessions := httptest.NewRecorder()
	h.ServeHTTP(sessions, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", strings.NewReader(`{"token":"`+s.secret+`"}`)))
	if sessions.Code != http.StatusOK {
		t.Fatalf("session: %d", sessions.Code)
	}
	for _, tc := range []struct{ name, host, origin, csrf string }{
		{"foreign host", "attacker.example:8080", "", "local-session"},
		{"foreign origin", "127.0.0.1:8080", "https://attacker.example", "local-session"},
		{"another local port", "127.0.0.1:8080", "http://127.0.0.1:8081", "local-session"},
		{"missing csrf", "127.0.0.1:8080", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/api/runs/create", strings.NewReader(`{}`))
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-CPGen-CSRF", tc.csrf)
			for _, c := range sessions.Result().Cookies() {
				r.AddCookie(c)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
