package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"github.com/gin-gonic/gin"
)

//go:embed static/*
var staticFiles embed.FS

type envelope struct {
	SchemaVersion string    `json:"schema_version"`
	Data          any       `json:"data,omitempty"`
	Error         *apiError `json:"error,omitempty"`
}
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type eventHistoryReader interface {
	EventsBefore(context.Context, domain.RunID, int64, int) ([]domain.RunEvent, error)
}

type Server struct {
	app                  *application.Application
	cfg                  config.Config
	manager              *taskManager
	secret               string
	mu                   sync.Mutex
	sessions             map[string]time.Time
	limiter              *http.Server
	similarityHTTPClient *http.Client
	closeFixture         func() error
	closeOnce            sync.Once
	closeErr             error
}

func New(ctx context.Context, cfg config.Config) (*Server, error) {
	return NewWithCapacity(ctx, cfg, 1)
}

func NewWithCapacity(ctx context.Context, cfg config.Config, capacity int) (*Server, error) {
	return newWithCapacity(ctx, cfg, capacity, nil)
}

func newWithCapacity(ctx context.Context, cfg config.Config, capacity int, fixture *localSimilarityFixture) (*Server, error) {
	if capacity < 1 || capacity > 64 {
		return nil, errors.New("capacity must be between 1 and 64")
	}
	app, err := application.BootstrapLocal(ctx, cfg)
	if err != nil {
		if fixture != nil {
			_ = fixture.Close()
		}
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		_ = app.Close()
		if fixture != nil {
			_ = fixture.Close()
		}
		return nil, err
	}
	server := &Server{app: app, cfg: cfg, manager: newTaskManager(capacity, configuredProviderSecrets(cfg)...), secret: base64.RawURLEncoding.EncodeToString(raw), sessions: map[string]time.Time{}}
	if fixture != nil {
		server.similarityHTTPClient = fixture.client
		server.closeFixture = fixture.Close
	}
	return server, nil
}

func configuredProviderSecrets(cfg config.Config) []string {
	names := make([]string, 0, 2)
	if cfg.LLM != nil {
		names = append(names, cfg.LLM.APIKeyEnv)
	}
	if cfg.Similarity != nil {
		names = append(names, cfg.Similarity.APIKeyEnv)
	}
	secrets := make([]string, 0, len(names))
	for _, name := range names {
		if secret := os.Getenv(name); secret != "" {
			secrets = append(secrets, secret)
		}
	}
	return secrets
}

func (s *Server) BootstrapURL(listen string) string {
	return "http://" + listen + "/?session=" + s.secret
}

func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), s.originHostGuard())
	r.Use(func(c *gin.Context) {
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'none'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		c.Next()
	})
	r.POST("/api/session", s.exchange)
	api := r.Group("/api", s.auth)
	api.GET("/runs", s.list)
	api.POST("/runs/create", s.create)
	api.GET("/runs/:id", s.detail)
	api.GET("/runs/:id/events", s.events)
	api.GET("/runs/:id/artifacts/:occurrence", s.artifact)
	api.POST("/runs/:id/resume", s.resume)
	api.POST("/runs/:id/cancel", s.cancelRun)
	api.POST("/runs/:id/review", s.review)
	api.GET("/runs/:id/package", s.packageFile)
	api.GET("/config", s.configSummary)
	r.GET("/app.css", func(c *gin.Context) { c.FileFromFS("static/app.css", http.FS(staticFiles)) })
	r.GET("/app.js", func(c *gin.Context) { c.FileFromFS("static/app.js", http.FS(staticFiles)) })
	r.GET("/app.js.LEGAL.txt", func(c *gin.Context) { c.FileFromFS("static/app.js.LEGAL.txt", http.FS(staticFiles)) })
	r.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(404, envelope{SchemaVersion: "cpgen.web/v1", Error: &apiError{Code: "not_found", Message: "接口不存在"}})
			return
		}
		body, err := staticFiles.ReadFile("static/index.html")
		if err != nil {
			c.Status(500)
			return
		}
		c.Data(200, "text/html; charset=utf-8", body)
	})
	return r
}

func (s *Server) Serve(ctx context.Context, listen string, onListen func(string)) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("serve only supports loopback addresses")
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, listener, onListen)
}

func (s *Server) ServeListener(ctx context.Context, listener net.Listener, onListen func(string)) error {
	if listener == nil {
		return errors.New("listener is required")
	}
	if onListen != nil {
		onListen(listener.Addr().String())
	}
	s.limiter = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- s.limiter.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		return s.Close()
	}
}

func (s *Server) Close() error {
	s.manager.beginDrain()
	s.manager.waitEmpty()
	s.closeOnce.Do(func() {
		var errs []error
		if s.limiter != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			errs = append(errs, s.limiter.Shutdown(ctx))
			cancel()
		}
		errs = append(errs, s.app.Close())
		if s.closeFixture != nil {
			errs = append(errs, s.closeFixture())
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

func (s *Server) originHostGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		host := c.Request.Host
		h, _, err := net.SplitHostPort(host)
		if err == nil {
			host = h
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			c.AbortWithStatusJSON(403, envelope{SchemaVersion: "cpgen.web/v1", Error: &apiError{Code: "invalid_host", Message: "仅允许本机访问"}})
			return
		}
		if origin := c.GetHeader("Origin"); origin != "" {
			u, err := http.NewRequest("GET", origin, nil)
			if err != nil || !strings.EqualFold(u.URL.Host, c.Request.Host) {
				c.AbortWithStatusJSON(403, envelope{SchemaVersion: "cpgen.web/v1", Error: &apiError{Code: "invalid_origin", Message: "来源不允许"}})
				return
			}
		}
		c.Next()
	}
}
func (s *Server) exchange(c *gin.Context) {
	var body struct {
		Token string `json:"token"`
	}
	if c.Request.ContentLength > 4096 {
		c.Status(413)
		return
	}
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096)).Decode(&body); err != nil || subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.secret)) != 1 {
		c.JSON(401, envelope{SchemaVersion: "cpgen.web/v1", Error: &apiError{Code: "invalid_session", Message: "会话凭证无效"}})
		return
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		c.Status(500)
		return
	}
	id := base64.RawURLEncoding.EncodeToString(buf)
	s.mu.Lock()
	for key, expiry := range s.sessions {
		if time.Now().After(expiry) {
			delete(s.sessions, key)
		}
	}
	s.sessions[id] = time.Now().Add(12 * time.Hour)
	s.mu.Unlock()
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("cpgen_session", id, 12*60*60, "/", "", false, true)
	c.Header("Cache-Control", "no-store")
	c.JSON(200, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]bool{"ok": true}})
}
func (s *Server) auth(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := c.Cookie("cpgen_session")
	if err != nil {
		c.AbortWithStatusJSON(401, envelope{SchemaVersion: "cpgen.web/v1", Error: &apiError{Code: "session_required", Message: "需要本机会话"}})
		return
	}
	s.mu.Lock()
	expiry, ok := s.sessions[id]
	if ok && time.Now().After(expiry) {
		delete(s.sessions, id)
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		c.AbortWithStatusJSON(401, envelope{SchemaVersion: "cpgen.web/v1", Error: &apiError{Code: "session_required", Message: "会话已失效，请使用服务启动时显示的链接"}})
		return
	}
	if c.Request.Method != "GET" && c.GetHeader("X-CPGen-CSRF") != "local-session" {
		c.AbortWithStatus(403)
		return
	}
	c.Next()
}
func (s *Server) list(c *gin.Context) {
	state := c.Query("state")
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeErr(c, 400, "invalid_argument", "limit 必须在 1 到 1000 之间")
			return
		}
		limit = n
	}
	query := port.WorkbenchRunQuery{State: state, Limit: limit, Cursor: c.Query("cursor")}
	if err := query.Validate(); err != nil {
		writeErr(c, 400, "invalid_state", "任务状态无效")
		return
	}
	reader, ok := s.app.Runtime.(port.WorkbenchRunListReader)
	if !ok {
		writeErr(c, 500, "unsupported_read", "任务列表读接口不可用")
		return
	}
	page, err := reader.WorkbenchRuns(c.Request.Context(), query)
	if err != nil {
		if errors.Is(err, port.ErrInvalidWorkbenchRunQuery) {
			writeErr(c, 400, "invalid_argument", "分页游标无效或与当前筛选条件不匹配")
			return
		}
		writeError(c, err)
		return
	}
	runRows := make([]runSummaryDTO, 0, len(page.Runs))
	for _, row := range page.Runs {
		dto := toRunSummaryDTO(row.RunSummary)
		dto.Brief = row.Brief
		runRows = append(runRows, dto)
	}
	c.JSON(200, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]any{"runs": runRows, "limit": limit, "scope": "recent", "next_cursor": page.NextCursor}})
}

func decodeBody(c *gin.Context, dst any) error {
	if c.Request.ContentLength > 1<<20 {
		return errors.New("request body exceeds limit")
	}
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func (s *Server) create(c *gin.Context) {
	var body createRequestBody
	if err := decodeBody(c, &body); err != nil {
		writeErr(c, 400, "invalid_request", "创建请求格式无效")
		return
	}
	request, err := decodeRunRequest(body.Request)
	if err != nil {
		writeErr(c, 422, "invalid_request", "题目或预算字段无效")
		return
	}
	if !validOperation(body.OperationKey) {
		writeErr(c, 422, "invalid_request", "操作身份无效，请重新提交")
		return
	}
	if err := application.ValidateCreateRequest(s.cfg, request); err != nil {
		writeApplicationError(c, err)
		return
	}
	if snapshot, exists, lookupErr := application.LookupCreate(c.Request.Context(), s.app, s.cfg, request, body.OperationKey); lookupErr != nil {
		writeApplicationError(c, lookupErr)
		return
	} else if exists {
		dto := toRunDTO(snapshot)
		c.JSON(202, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]any{"run": dto, "run_id": dto.RunID, "accepted": true, "idempotent_replay": true}})
		return
	}
	reservation, err := s.manager.reserve()
	if err != nil {
		writeManagerError(c, err)
		return
	}
	defer reservation.release()
	snapshot, created, err := application.CreateOnly(c.Request.Context(), s.app, s.cfg, request, body.OperationKey)
	if err != nil {
		writeApplicationError(c, err)
		return
	}
	if !created {
		dto := toRunDTO(snapshot)
		c.JSON(202, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]any{"run": dto, "run_id": dto.RunID, "accepted": true, "idempotent_replay": true}})
		return
	}
	if err := reservation.start(snapshot.RunID, func(ctx context.Context) error { return s.execute(ctx, snapshot.RunID) }); err != nil {
		writeManagerError(c, err)
		return
	}
	dto := toRunDTO(snapshot)
	c.JSON(202, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]any{"run": dto, "run_id": dto.RunID, "accepted": true, "idempotent_replay": false}})
}

func (s *Server) execute(ctx context.Context, id domain.RunID) error {
	return s.executeExpected(ctx, id, nil)
}

func parseBudget(b budgetDTO) (domain.BudgetLimits, error) {
	tokenBudget, err := b.usesTokenBudget()
	if err != nil {
		return domain.BudgetLimits{}, err
	}
	if tokenBudget {
		tokens, err := strconv.ParseInt(b.MaxLLMTokens, 10, 64)
		if err != nil || tokens < 0 {
			return domain.BudgetLimits{}, errors.New("max_llm_tokens must be non-negative integer string")
		}
		return domain.BudgetLimits{MaxLLMTokens: tokens}, nil
	}
	parse := func(name, value string) (int64, error) {
		if value == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return 0, errors.New(name + " must be non-negative integer string")
		}
		return n, nil
	}
	if b.usesSplitTokenBudget() {
		var increase domain.BudgetLimits
		for _, field := range []struct {
			name, value string
			dst         *int64
		}{
			{"max_llm_input_tokens", b.MaxLLMInputTokens, &increase.MaxLLMInputTokens},
			{"max_llm_output_tokens", b.MaxLLMOutputTokens, &increase.MaxLLMOutputTokens},
		} {
			_, present := b.fields[field.name]
			if !present && field.value == "" {
				continue
			}
			if field.value == "" {
				return domain.BudgetLimits{}, errors.New(field.name + " must be non-negative integer string")
			}
			*field.dst, err = parse(field.name, field.value)
			if err != nil {
				return domain.BudgetLimits{}, err
			}
		}
		return increase, nil
	}
	var v domain.BudgetLimits
	items := []struct {
		name, value string
		dst         *int64
		money       bool
	}{{"max_llm_calls", b.MaxLLMCalls, &v.MaxLLMCalls, false}, {"max_similarity_calls", b.MaxSimilarityCalls, &v.MaxSimilarityCalls, false}, {"max_llm_input_tokens", b.MaxLLMInputTokens, &v.MaxLLMInputTokens, false}, {"max_llm_output_tokens", b.MaxLLMOutputTokens, &v.MaxLLMOutputTokens, false}, {"max_llm_cost_usd", b.MaxLLMCostUSD, &v.MaxLLMCostMicroUSD, true}, {"max_similarity_cost_usd", b.MaxSimilarityCostUSD, &v.MaxSimilarityCostMicroUSD, true}, {"max_sandbox_creates", b.MaxSandboxCreates, &v.MaxSandboxCreates, false}, {"max_artifact_bytes", b.MaxArtifactBytes, &v.MaxArtifactBytes, false}, {"max_package_bytes", b.MaxPackageBytes, &v.MaxPackageBytes, false}, {"max_mutations_per_stage", b.MaxMutationsPerStage, &v.MaxMutationsPerStage, false}, {"max_active_time_milliseconds", b.MaxActiveTimeMilliseconds, &v.MaxActiveTimeMilliseconds, false}}
	for _, i := range items {
		if i.money {
			if i.value != "" {
				*i.dst, err = parseUSDmicro(i.value)
			}
		} else {
			*i.dst, err = parse(i.name, i.value)
		}
		if err != nil {
			return v, err
		}
	}
	return v, nil
}

func operationID(kind, key string) string {
	digest := domain.SumBytes([]byte("cpgen.web." + kind + "/v1:" + key))
	return kind + "_" + string(digest[len("sha256:"):len("sha256:")+32])
}
func writeManagerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrCapacityFull):
		writeErr(c, 503, "capacity_full", "本地执行容量已满")
	case errors.Is(err, ErrDraining):
		writeErr(c, 503, "server_draining", "服务正在退出")
	case errors.Is(err, ErrRunActive):
		writeErr(c, 409, "executor_active", "该任务已有执行器")
	default:
		writeErr(c, 500, "internal_error", "无法接纳任务")
	}
}
func writeApplicationError(c *gin.Context, err error) {
	if errors.Is(err, application.ErrInvalidCreateRequest) {
		writeErr(c, 422, "invalid_request", "题目要求、算法标签或预算字段无效，请修改后重试")
		return
	}
	if errors.Is(err, sqlite.ErrVersionConflict) || errors.Is(err, application.ErrRunVersionConflict) {
		writeErr(c, 409, "version_conflict", "任务版本已变化，请刷新后重试")
		return
	}
	if errors.Is(err, sqlite.ErrInvalidTransition) || errors.Is(err, sqlite.ErrReviewPending) || errors.Is(err, sqlite.ErrCancelPending) {
		writeErr(c, 409, "state_conflict", "当前状态不允许该操作或已有待处理请求")
		return
	}
	if errors.Is(err, runlock.ErrBusy) {
		writeErr(c, 409, "executor_active", "该任务已有执行器")
		return
	}
	if errors.Is(err, application.ErrCreateIdempotencyConflict) {
		writeErr(c, 409, "idempotency_conflict", "操作身份已用于不同请求")
		return
	}
	writeError(c, err)
}
func (s *Server) events(c *gin.Context) {
	id := domain.RunID(c.Param("id"))
	if err := id.Validate(); err != nil {
		writeErr(c, 400, "invalid_id", "任务编号无效")
		return
	}
	if _, err := s.app.Runtime.GetRun(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	before := int64(0)
	if raw := c.Query("before"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeErr(c, 400, "invalid_argument", "before 无效")
			return
		}
		before = n
	}
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n < 1 || n > 200 {
			writeErr(c, 400, "invalid_argument", "limit 必须在 1 到 200 之间")
			return
		}
		limit = n
	}
	rows, err := s.recentEvents(c.Request.Context(), id, before, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	eventRows := make([]eventDTO, 0, len(rows))
	for _, event := range rows {
		eventRows = append(eventRows, toEventDTO(event))
	}
	next := int64(0)
	if len(rows) > 0 {
		next = rows[0].Version
	}
	c.JSON(200, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]any{"events": eventRows, "before_version": strconv.FormatInt(next, 10)}})
}

func (s *Server) recentEvents(ctx context.Context, id domain.RunID, before int64, limit int) ([]domain.RunEvent, error) {
	reader, ok := s.app.Runtime.(eventHistoryReader)
	if !ok {
		return nil, errors.New("event history is not supported by runtime store")
	}
	return reader.EventsBefore(ctx, id, before, limit)
}
func (s *Server) packageFile(c *gin.Context) {
	id := domain.RunID(c.Param("id"))
	if err := id.Validate(); err != nil {
		writeErr(c, 400, "invalid_id", "任务编号无效")
		return
	}
	archive, _, err := s.app.Packages.ReadArchive(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", string(id)+".zip"))
	c.Header("Content-Length", strconv.Itoa(len(archive)))
	c.Data(200, "application/zip", archive)
}
func (s *Server) configSummary(c *gin.Context) {
	e, err := s.cfg.EffectiveConfig()
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]any{"source": "startup", "config": losslessJSON(e)}})
}
func writeErr(c *gin.Context, status int, code, message string) {
	c.JSON(status, envelope{SchemaVersion: "cpgen.web/v1", Error: &apiError{Code: code, Message: message}})
}
func writeError(c *gin.Context, err error) {
	status := 500
	code := "internal_error"
	msg := "操作失败"
	if errors.Is(err, sqlite.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		status = 404
		code = "not_found"
		msg = "任务不存在"
	}
	writeErr(c, status, code, msg)
}
