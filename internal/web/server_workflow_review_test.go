package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpgen/internal/config"
	"cpgen/internal/domain"
)

func TestHTTPCreateReplayUsesOnePersistedRun(t *testing.T) {
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
	sessionBody, _ := json.Marshal(map[string]string{"token": server.secret})
	exchange := httptest.NewRecorder()
	exchangeRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", bytes.NewReader(sessionBody))
	exchangeRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(exchange, exchangeRequest)
	if exchange.Code != http.StatusOK {
		t.Fatalf("session exchange: HTTP %d", exchange.Code)
	}
	cookies := exchange.Result().Cookies()
	doRequest := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CPGen-CSRF", "local-session")
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// Large seeds and budget values are strings at the Web boundary. The real
	// Fake workflow exercises persistence and execution without a paid provider.
	const body = `{"operation_key":"http-create-replay-review","request":{"schema_version":"cpgen.request/v1","mode":"manual","brief":"HTTP review fixture","tags":[],"normalized_tags":[],"language":"en","difficulty":"easy","required_features":[],"forbidden_features":[],"time_limit_milliseconds":"1000","memory_limit_megabytes":"64","solution_language":"cpp","seed":"9007199254740993","verification_profile":"default","export_targets":["internal"],"budget_limits":{"max_llm_calls":"0","max_similarity_calls":"0","max_llm_input_tokens":"0","max_llm_output_tokens":"0","max_llm_cost_usd":"0","max_similarity_cost_usd":"0","max_sandbox_creates":"0","max_artifact_bytes":"0","max_package_bytes":"0","max_mutations_per_stage":"0","max_active_time_milliseconds":"5000"}}}`
	created := doRequest(http.MethodPost, "/api/runs/create", body)
	if created.Code != http.StatusAccepted {
		t.Fatalf("create: HTTP %d: %s", created.Code, created.Body.String())
	}
	var result struct {
		Data struct {
			RunID string `json:"run_id"`
			Run   struct {
				RunID string `json:"run_id"`
			} `json:"run"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	id := result.Data.RunID
	if id == "" {
		id = result.Data.Run.RunID
	}
	if id == "" {
		t.Fatal("create response contains no run identity")
	}
	deadline := time.Now().Add(5 * time.Second)
	for server.manager.isActive(domain.RunID(id)) {
		if time.Now().After(deadline) {
			t.Fatal("Fake executor did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(id))
	if err != nil {
		t.Fatal(err)
	}
	if before.State != domain.RunNeedsReview {
		t.Fatalf("Fake workflow did not reach its review checkpoint: %s", before.State)
	}
	detail := doRequest(http.MethodGet, "/api/runs/"+id, "")
	if detail.Code != http.StatusOK {
		t.Fatalf("detail: HTTP %d: %s", detail.Code, detail.Body.String())
	}
	var detailResult struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &detailResult); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"run", "stages", "budget", "execution", "review", "available_actions", "artifacts", "recent_events"} {
		if len(detailResult.Data[key]) == 0 {
			t.Errorf("aggregate detail missing %s", key)
		}
	}
	replayed := doRequest(http.MethodPost, "/api/runs/create", body)
	if replayed.Code != http.StatusOK && replayed.Code != http.StatusAccepted {
		t.Fatalf("idempotent replay: HTTP %d: %s", replayed.Code, replayed.Body.String())
	}
	if server.manager.isActive(domain.RunID(id)) {
		t.Fatal("replaying create started another executor")
	}
	after, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(id))
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || after.State != before.State {
		t.Fatalf("replaying create mutated the run: before=%+v after=%+v", before, after)
	}
	conflict := doRequest(http.MethodPost, "/api/runs/create", strings.Replace(body, "HTTP review fixture", "changed request", 1))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same identity with changed request must conflict: HTTP %d: %s", conflict.Code, conflict.Body.String())
	}
	rows, err := server.app.Runtime.ListRuns(context.Background(), domain.RunFilter{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("create/replay/conflict persisted unexpected runs: count=%d err=%v", len(rows), err)
	}
	reviewBody := fmt.Sprintf(`{"operation_key":"reject-once","expected_run_version":"%d","workflow_revision":%q,"kind":"REJECT","reviewer":"test","reason":"not accepted"}`, after.Version, after.WorkflowRevision)
	for i := 0; i < 2; i++ {
		response := doRequest(http.MethodPost, "/api/runs/"+id+"/review", reviewBody)
		if response.Code != 201 {
			t.Fatalf("review %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	current, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(id))
	if err != nil {
		t.Fatal(err)
	}
	if current.State != domain.RunNeedsReview {
		t.Fatal("review must not automatically execute")
	}
	if response := doRequest(http.MethodGet, "/api/runs/"+id, ""); response.Code != 200 {
		t.Fatalf("pending detail: %d %s", response.Code, response.Body.String())
	}
	resumeBody := fmt.Sprintf(`{"operation_key":"resume-once","expected_run_version":"%d"}`, current.Version)
	response := doRequest(http.MethodPost, "/api/runs/"+id+"/resume", resumeBody)
	if response.Code != 202 {
		t.Fatalf("resume: %d %s", response.Code, response.Body.String())
	}
	awaitManagerEmpty(t, server.manager)
	finished, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(id))
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != domain.RunFailed {
		t.Fatalf("reject not applied: %+v; execution=%+v", finished, server.manager.observe(domain.RunID(id)))
	}
	// Durable receipts remain effective after the serving process is replaced.
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	server, err = New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	handler = server.Handler()
	sessionBody, _ = json.Marshal(map[string]string{"token": server.secret})
	exchange = httptest.NewRecorder()
	handler.ServeHTTP(exchange, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", bytes.NewReader(sessionBody)))
	cookies = exchange.Result().Cookies()
	for _, pair := range [][2]string{{"resume", resumeBody}, {"review", reviewBody}} {
		response = doRequest(http.MethodPost, "/api/runs/"+id+"/"+pair[0], pair[1])
		if response.Code != 201 && response.Code != 202 {
			t.Fatalf("restart replay %s: %d %s", pair[0], response.Code, response.Body.String())
		}
	}
	if server.manager.isActive(domain.RunID(id)) {
		t.Fatal("replay restarted executor")
	}
	// A separate task can be cancelled while waiting for review, then retried safely.
	response = doRequest(http.MethodPost, "/api/runs/create", strings.Replace(body, "http-create-replay-review", "cancel-fixture", 1))
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	_ = json.Unmarshal(response.Body.Bytes(), &result)
	cancelID := result.Data.RunID
	awaitManagerEmpty(t, server.manager)
	cancelRun, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(cancelID))
	if err != nil {
		t.Fatal(err)
	}
	cancelBody := fmt.Sprintf(`{"operation_key":"cancel-once","expected_run_version":"%d","reason":"test complete"}`, cancelRun.Version)
	for i := 0; i < 2; i++ {
		response = doRequest(http.MethodPost, "/api/runs/"+cancelID+"/cancel", cancelBody)
		if response.Code != 202 {
			t.Fatalf("cancel: %d %s", response.Code, response.Body.String())
		}
		awaitManagerEmpty(t, server.manager)
	}
	cancelled, err := server.app.Runtime.GetRun(context.Background(), domain.RunID(cancelID))
	if err != nil || cancelled.State != domain.RunCancelled {
		t.Fatalf("cancelled: %+v %v", cancelled, err)
	}
}
