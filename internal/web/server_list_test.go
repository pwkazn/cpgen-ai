package web

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"github.com/gin-gonic/gin"
)

func TestHTTPRunListPaginationAndValidation(t *testing.T) {
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.ToSlash(filepath.Join(t.TempDir(), "state")) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	request := domain.RunRequest{
		SchemaVersion: "cpgen.request/v1", Mode: "manual", Brief: "paginated HTTP list", Language: "en", Difficulty: "easy",
		TimeLimitMilliseconds: 1000, MemoryLimitMegabytes: 64, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"},
		BudgetLimits: domain.BudgetLimits{MaxActiveTimeMilliseconds: 5000},
	}
	for i := 0; i < 3; i++ {
		if _, _, err := application.CreateOnly(context.Background(), server.app, cfg, request, "list-run-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	handler := server.Handler()
	body, _ := json.Marshal(map[string]string{"token": server.secret})
	exchange := httptest.NewRecorder()
	auth := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", bytes.NewReader(body))
	auth.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(exchange, auth)
	if exchange.Code != http.StatusOK {
		t.Fatalf("session exchange: %d", exchange.Code)
	}
	get := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/runs"+query, nil)
		for _, cookie := range exchange.Result().Cookies() {
			r.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		return response
	}
	type listData struct {
		Runs       []runSummaryDTO `json:"runs"`
		Limit      int             `json:"limit"`
		NextCursor *string         `json:"next_cursor"`
	}
	decode := func(response *httptest.ResponseRecorder) listData {
		if response.Code != http.StatusOK {
			t.Fatalf("list response: %d %s", response.Code, response.Body.String())
		}
		var envelope struct {
			Data listData `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Data.Runs == nil || envelope.Data.NextCursor == nil {
			t.Fatalf("list must return an array and explicit cursor: %s", response.Body.String())
		}
		return envelope.Data
	}
	first := decode(get("?state=active&limit=1"))
	if len(first.Runs) != 1 || first.Limit != 1 || *first.NextCursor == "" || first.Runs[0].Brief != request.Brief {
		t.Fatalf("first page: %+v", first)
	}
	tail := decode(get("?state=active&limit=2&cursor=" + url.QueryEscape(*first.NextCursor)))
	if len(tail.Runs) != 2 || *tail.NextCursor != "" || tail.Limit != 2 {
		t.Fatalf("tail page: %+v", tail)
	}
	seen := map[domain.RunID]bool{first.Runs[0].RunID: true}
	for _, run := range tail.Runs {
		if seen[run.RunID] {
			t.Fatalf("duplicate paged run: %s", run.RunID)
		}
		seen[run.RunID] = true
	}
	for _, filter := range []string{"", "CREATED", "RUNNING", "BLOCKED", "NEEDS_REVIEW", "FAILED", "CANCELLED", "READY", "active", "ended", "ready", "blocked"} {
		page := decode(get("?state=" + filter))
		wantCount := 0
		if filter == "" || filter == "CREATED" || filter == "active" {
			wantCount = 3
		}
		if page.Limit != 50 || len(page.Runs) != wantCount || *page.NextCursor != "" {
			t.Fatalf("filter=%q default page: %+v", filter, page)
		}
	}
	for _, query := range []string{
		"?state=ALL", "?state=running", "?state=CREATED,RUNNING", "?limit=0", "?limit=-1", "?limit=1001", "?limit=abc", "?limit=9223372036854775807", "?cursor=invalid",
		"?state=ended&cursor=" + url.QueryEscape(*first.NextCursor),
		"?cursor=" + url.QueryEscape(*first.NextCursor),
	} {
		response := get(query)
		if response.Code != http.StatusBadRequest {
			t.Errorf("query %q: expected 400, got %d %s", query, response.Code, response.Body.String())
		}
	}
}

type runListProjectionStore struct {
	port.RuntimeStore
	query port.WorkbenchRunQuery
	page  port.WorkbenchRunPage
}

func (s *runListProjectionStore) WorkbenchRuns(_ context.Context, q port.WorkbenchRunQuery) (port.WorkbenchRunPage, error) {
	s.query = q
	return s.page, nil
}

func TestHTTPRunListPreservesVersionAndTimestampPrecision(t *testing.T) {
	at := time.Date(2026, 9, 1, 8, 0, 0, 123456789, time.UTC)
	store := &runListProjectionStore{page: port.WorkbenchRunPage{Runs: []port.WorkbenchRunSummary{{
		RunSummary: domain.RunSummary{RunID: "run_ffffffffffffffffffffffffffffffff", State: domain.RunCreated, Version: math.MaxInt64, CurrentStage: "idea", CreatedAt: at, UpdatedAt: at},
		Brief:      "precise projection",
	}}, NextCursor: "opaque-next-page"}}
	server := &Server{app: &application.Application{Runtime: store}}
	router := gin.New()
	router.GET("/api/runs", server.list)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/runs?state=active&limit=2&cursor=opaque-page", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list response: %d %s", response.Code, response.Body.String())
	}
	if store.query != (port.WorkbenchRunQuery{State: "active", Limit: 2, Cursor: "opaque-page"}) {
		t.Fatalf("list query lost pagination/filter: %+v", store.query)
	}
	var result struct {
		Data struct {
			Runs       []map[string]string `json:"runs"`
			NextCursor string              `json:"next_cursor"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("identities/version/timestamps must be JSON strings: %v", err)
	}
	if len(result.Data.Runs) != 1 || result.Data.Runs[0]["version"] != "9223372036854775807" || result.Data.Runs[0]["run_id"] != "run_ffffffffffffffffffffffffffffffff" || result.Data.Runs[0]["updated_at"] != "2026-09-01T08:00:00.123456789Z" || result.Data.NextCursor != "opaque-next-page" {
		t.Fatalf("list precision or cursor lost: %s", response.Body.String())
	}
}
