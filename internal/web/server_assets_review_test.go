package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/config"
)

func TestEmbeddedWorkbenchAssetsAndDeepLinks(t *testing.T) {
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.ToSlash(filepath.Join(t.TempDir(), "state")) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	handler := server.Handler()
	for _, tc := range []struct {
		path, mediaType string
	}{
		{"/", "text/html"},
		{"/runs", "text/html"},
		{"/runs/run_00000000000000000000000000000031", "text/html"},
		{"/app.css", "text/css"},
		{"/app.js", "javascript"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+tc.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("%s: HTTP %d: %s", tc.path, response.Code, response.Body.String())
			}
			if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, tc.mediaType) {
				t.Fatalf("%s: content type %q does not contain %q", tc.path, contentType, tc.mediaType)
			}
			if response.Body.Len() == 0 {
				t.Fatalf("%s: empty asset", tc.path)
			}
		})
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/runs", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("private API accessible without a session: HTTP %d", response.Code)
	}
}

func TestAuthenticatedEmptyRunListIsAnArray(t *testing.T) {
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.ToSlash(filepath.Join(t.TempDir(), "state")) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	handler := server.Handler()
	body, err := json.Marshal(map[string]string{"token": server.secret})
	if err != nil {
		t.Fatal(err)
	}
	exchange := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(exchange, request)
	if exchange.Code != http.StatusOK {
		t.Fatalf("session exchange: HTTP %d", exchange.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/runs", nil)
	for _, cookie := range exchange.Result().Cookies() {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("run list: HTTP %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Data struct {
			Runs json.RawMessage `json:"runs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if string(result.Data.Runs) != "[]" {
		t.Fatalf("new workspace must return an empty array, got %s", result.Data.Runs)
	}
}
