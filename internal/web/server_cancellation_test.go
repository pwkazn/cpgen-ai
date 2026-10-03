package web

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/toolchain"
)

func TestHTTPCancelUnstartedRunAfterDockerBootstrapFailure(t *testing.T) {
	server, run, daemonCalls := unstartedCancellationServer(t, nil)
	if err := server.execute(context.Background(), run.RunID); err == nil {
		t.Fatal("unavailable Docker fixture unexpectedly bootstrapped")
	}
	before := daemonCalls.Load()
	if before == 0 {
		t.Fatal("fixture failed before checking Docker")
	}
	// A local cancellation must also work after provider credentials disappear.
	t.Setenv(server.cfg.LLM.APIKeyEnv, "")
	t.Setenv(server.cfg.Similarity.APIKeyEnv, "")
	post := cancellationPoster(t, server)
	body := fmt.Sprintf(`{"operation_key":"cancel-unstarted","expected_run_version":"%d","reason":"stop before generation"}`, run.Version)
	for i := 0; i < 2; i++ {
		response := post(run.RunID, body)
		if response.Code != http.StatusAccepted {
			t.Fatalf("cancel %d: HTTP %d: %s", i, response.Code, response.Body.String())
		}
		awaitManagerEmpty(t, server.manager)
		assertCancelledUnstarted(t, server, run.RunID)
	}
	if got := daemonCalls.Load(); got != before {
		t.Fatalf("cancellation contacted Docker: before=%d after=%d", before, got)
	}
}

func TestHTTPCancelFollowsExecutorStillBootstrapping(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var enteredOnce sync.Once
	var bootstrapCalls atomic.Int32
	unblock := func() { once.Do(func() { close(release) }) }
	server, run, daemonCalls := unstartedCancellationServer(t, func() {
		enteredOnce.Do(func() { close(entered) })
		<-release
	})
	// Registered last: unblock a failed test before Server.Close joins the task.
	t.Cleanup(unblock)
	reservation, err := server.manager.reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.start(run.RunID, func(ctx context.Context) error {
		err := server.executeExpected(ctx, run.RunID, &run.Version)
		bootstrapCalls.Store(daemonCalls.Load())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not enter Docker bootstrap")
	}
	post := cancellationPoster(t, server)
	body := fmt.Sprintf(`{"operation_key":"cancel-during-bootstrap","expected_run_version":"%d","reason":"stop while initializing"}`, run.Version)
	for i := 0; i < 2; i++ {
		if response := post(run.RunID, body); response.Code != http.StatusAccepted {
			t.Fatalf("cancel: HTTP %d: %s", response.Code, response.Body.String())
		}
	}
	pending, err := server.app.Runtime.PendingCancel(context.Background(), run.RunID)
	if err != nil || pending == nil {
		t.Fatalf("cancellation was not durable during bootstrap: %+v %v", pending, err)
	}
	unblock()
	awaitManagerEmpty(t, server.manager)
	assertCancelledUnstarted(t, server, run.RunID)
	if got := daemonCalls.Load(); got != bootstrapCalls.Load() {
		t.Fatalf("follow-up cancellation contacted Docker: bootstrap=%d final=%d", bootstrapCalls.Load(), got)
	}
	if observation := server.manager.observe(run.RunID); observation.Active || observation.LastError != "" {
		t.Fatalf("successful cancellation retained bootstrap failure: %+v", observation)
	}
}

func TestExecuteHonorsPendingUnstartedCancelBeforeBootstrap(t *testing.T) {
	server, run, daemonCalls := unstartedCancellationServer(t, nil)
	_, err := server.app.Runtime.RequestCancel(context.Background(), domain.CancelRequest{
		ID: domain.ControlRequestID(operationID("control", "before-execute")), RunID: run.RunID,
		ExpectedRunVersion: run.Version, Reason: "cancel before bootstrap", IdempotencyKey: operationID("control", "before-execute"), At: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(server.cfg.LLM.APIKeyEnv, "")
	t.Setenv(server.cfg.Similarity.APIKeyEnv, "")
	if err := server.executeExpected(context.Background(), run.RunID, &run.Version); err != nil {
		t.Fatalf("pending cancellation did not precede execution: %v", err)
	}
	assertCancelledUnstarted(t, server, run.RunID)
	if got := daemonCalls.Load(); got != 0 {
		t.Fatalf("cancelled executor contacted Docker: %d requests", got)
	}
}

func TestHTTPCancelDuringDrainDoesNotPersistUntrackedRequest(t *testing.T) {
	server, run, _ := unstartedCancellationServer(t, nil)
	post := cancellationPoster(t, server)
	server.manager.beginDrain()
	body := fmt.Sprintf(`{"operation_key":"cancel-after-drain","expected_run_version":"%d","reason":"stop"}`, run.Version)
	response := post(run.RunID, body)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancel during drain: HTTP %d: %s", response.Code, response.Body.String())
	}
	pending, err := server.app.Runtime.PendingCancel(context.Background(), run.RunID)
	if err != nil || pending != nil {
		t.Fatalf("drain left an untracked cancellation: %+v %v", pending, err)
	}
}

func assertCancelledUnstarted(t *testing.T, server *Server, id domain.RunID) {
	t.Helper()
	run, err := server.app.Runtime.GetRun(context.Background(), id)
	if err != nil || run.State != domain.RunCancelled {
		t.Fatalf("unstarted cancellation: %+v %v", run, err)
	}
	events, err := server.app.Runtime.Events(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[1].Type != domain.EventCancelRequested || events[2].Type != domain.EventCancelFinalized {
		t.Fatalf("cancellation started work or replayed events: %+v", events)
	}
}

func cancellationPoster(t *testing.T, server *Server) func(domain.RunID, string) *httptest.ResponseRecorder {
	t.Helper()
	handler := server.Handler()
	session := httptest.NewRecorder()
	handler.ServeHTTP(session, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/session", strings.NewReader(`{"token":"`+server.secret+`"}`)))
	if session.Code != http.StatusOK {
		t.Fatalf("session: HTTP %d %s", session.Code, session.Body.String())
	}
	return func(id domain.RunID, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/runs/"+string(id)+"/cancel", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CPGen-CSRF", "local-session")
		for _, cookie := range session.Result().Cookies() {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
}

func unstartedCancellationServer(t *testing.T, beforeDockerError func()) (*Server, domain.RunSnapshot, *atomic.Int32) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a Unix socket; no live Docker daemon is required")
	}
	root := t.TempDir()
	// Keep the Unix socket pathname below the platform limit even for long test names.
	socketDir, err := os.MkdirTemp("", "cpgen-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
	daemon := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if beforeDockerError != nil {
			beforeDockerError()
		}
		http.Error(w, "Docker unavailable in test", http.StatusServiceUnavailable)
	})}
	go func() { _ = daemon.Serve(listener) }()
	t.Cleanup(func() { _ = daemon.Close() })
	raw, err := os.ReadFile("../../config/toolchains/docker-v1.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := toolchain.LoadLock(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := lock.Digest()
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "lock.json")
	if err := os.WriteFile(lockPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../config/mvp.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.NewReplacer(
		"D:/cpgen-private/mvp", filepath.ToSlash(filepath.Join(root, "state")),
		"D:/cpgen-private/toolchains/docker-v1.lock.json", filepath.ToSlash(lockPath),
		"sha256:"+strings.Repeat("0", 64), string(digest),
		"npipe:////./pipe/docker_engine", "unix://"+socket,
	).Replace(string(example))
	cfg, err := config.Decode([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(cfg.LLM.APIKeyEnv, "fixture-key")
	t.Setenv(cfg.Similarity.APIKeyEnv, "fixture-key")
	server, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	request := domain.RunRequest{SchemaVersion: domain.RequestSchemaV1, Mode: domain.RequestModeManual, Brief: "Unstarted cancellation fixture", Tags: []string{"graphs"}, NormalizedTags: []string{"graphs"}, Language: "en", Difficulty: "easy", TimeLimitMilliseconds: 1000, MemoryLimitMegabytes: 64, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"}}
	run, _, err := application.CreateOnly(context.Background(), server.app, cfg, request, "unstarted-cancel-fixture")
	if err != nil {
		t.Fatal(err)
	}
	// Resume/cancel must use the persisted snapshot, even if this file is gone.
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	return server, run, calls
}
