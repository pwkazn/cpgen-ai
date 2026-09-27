package web

import (
	"context"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/runlock"
	"database/sql"
	"errors"
	"github.com/gin-gonic/gin"
	"strings"
	"time"
)

type workbenchCommands interface {
	LookupWorkbenchResume(context.Context, domain.RunID, string, int64) (bool, error)
	ClaimWorkbenchResume(context.Context, domain.RunID, string, int64) (bool, error)
	WorkbenchReview(context.Context, domain.RunID, domain.ReviewDecisionID) (domain.ReviewDecision, error)
	WorkbenchReviewBinding(context.Context, domain.RunID, domain.StageName) (domain.Digest, domain.Digest, domain.Digest, error)
	WorkbenchCancel(context.Context, domain.RunID, string) (domain.CancelRequest, error)
}

func validOperation(key string) bool { return strings.TrimSpace(key) != "" && len(key) <= 256 }

func (s *Server) resume(c *gin.Context) {
	var body struct {
		OperationKey    string `json:"operation_key"`
		ExpectedVersion int64  `json:"expected_run_version,string"`
	}
	if decodeBody(c, &body) != nil || !validOperation(body.OperationKey) || body.ExpectedVersion < 1 {
		writeErr(c, 400, "invalid_request", "恢复请求需要操作身份和任务版本")
		return
	}
	id := domain.RunID(c.Param("id"))
	if id.Validate() != nil {
		writeErr(c, 400, "invalid_id", "任务编号无效")
		return
	}
	store := s.app.Runtime.(workbenchCommands)
	replay, err := store.LookupWorkbenchResume(c.Request.Context(), id, body.OperationKey, body.ExpectedVersion)
	if err != nil {
		writeApplicationError(c, err)
		return
	}
	if !replay {
		reservation, err := s.manager.reserve()
		if err != nil {
			writeManagerError(c, err)
			return
		}
		defer reservation.release()
		guard, err := s.app.Locks.TryAcquireRun(id, runlock.Exclusive)
		if err != nil {
			writeApplicationError(c, err)
			return
		}
		replay, err = store.ClaimWorkbenchResume(c.Request.Context(), id, body.OperationKey, body.ExpectedVersion)
		_ = guard.Close()
		if err != nil {
			writeApplicationError(c, err)
			return
		}
		if !replay {
			if err = reservation.start(id, func(ctx context.Context) error { return s.executeExpected(ctx, id, &body.ExpectedVersion) }); err != nil {
				writeManagerError(c, err)
				return
			}
		}
	}
	c.JSON(202, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]any{"run_id": id, "accepted": true, "idempotent_replay": replay}})
}

func (s *Server) cancelRun(c *gin.Context) {
	var body struct {
		OperationKey    string `json:"operation_key"`
		ExpectedVersion int64  `json:"expected_run_version,string"`
		Reason          string `json:"reason"`
	}
	if decodeBody(c, &body) != nil || !validOperation(body.OperationKey) || body.ExpectedVersion < 1 || strings.TrimSpace(body.Reason) == "" {
		writeErr(c, 400, "invalid_request", "取消请求需要操作身份、任务版本和原因")
		return
	}
	id := domain.RunID(c.Param("id"))
	if id.Validate() != nil {
		writeErr(c, 400, "invalid_id", "任务编号无效")
		return
	}
	key := operationID("control", body.OperationKey)
	req := domain.CancelRequest{ID: domain.ControlRequestID(key), RunID: id, ExpectedRunVersion: body.ExpectedVersion, Reason: strings.TrimSpace(body.Reason), IdempotencyKey: key, At: time.Now().UTC()}
	previous, err := s.app.Runtime.(workbenchCommands).WorkbenchCancel(c.Request.Context(), id, key)
	if err == nil {
		if previous.Reason != req.Reason || previous.ExpectedRunVersion != req.ExpectedRunVersion {
			writeErr(c, 409, "idempotency_conflict", "操作身份已用于不同请求")
			return
		}
		req = previous
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeError(c, err)
		return
	}
	if _, err = s.app.Runtime.RequestCancel(context.Background(), req); err != nil {
		writeApplicationError(c, err)
		return
	}
	// Active executors observe the durable control request. An idle task gets a
	// tracked cleanup goroutine, independent of generation capacity and HTTP life.
	if !s.manager.isActive(id) {
		reservation, e := s.manager.reserveControl()
		if e == nil {
			defer reservation.release()
			_ = reservation.start(id, func(ctx context.Context) error {
				app, err := s.executionApp(ctx, id)
				if err != nil {
					return err
				}
				defer app.Close()
				_, err = app.Runs.Cancel(ctx, req)
				return err
			})
		}
	}
	snapshot, err := s.app.Runtime.GetRun(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(202, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]any{"run": toRunDTO(snapshot), "cancel_requested": snapshot.State != domain.RunCancelled}})
}

func (s *Server) review(c *gin.Context) {
	var body struct {
		OperationKey     string                    `json:"operation_key"`
		ExpectedVersion  int64                     `json:"expected_run_version,string"`
		WorkflowRevision string                    `json:"workflow_revision"`
		Kind             domain.ReviewDecisionKind `json:"kind"`
		Reviewer         string                    `json:"reviewer"`
		Reason           string                    `json:"reason"`
		BudgetIncrease   *budgetDTO                `json:"budget_increase,omitempty"`
	}
	if decodeBody(c, &body) != nil || !validOperation(body.OperationKey) || body.ExpectedVersion < 1 || strings.TrimSpace(body.Reviewer) == "" || strings.TrimSpace(body.Reason) == "" {
		writeErr(c, 400, "invalid_request", "评审需要任务版本、工作流版本、评审人和理由")
		return
	}
	if body.Kind != domain.ReviewRetry && body.Kind != domain.ReviewReject {
		writeErr(c, 422, "invalid_review_kind", "仅支持重试或拒绝")
		return
	}
	var increase domain.BudgetLimits
	var err error
	if body.BudgetIncrease != nil {
		increase, err = parseBudget(*body.BudgetIncrease)
		if err != nil {
			writeErr(c, 422, "invalid_budget", "预算增量格式无效")
			return
		}
	}
	if body.Kind == domain.ReviewReject && increase != (domain.BudgetLimits{}) {
		writeErr(c, 422, "invalid_review_payload", "拒绝决定不能增加预算")
		return
	}
	id := domain.RunID(c.Param("id"))
	if id.Validate() != nil {
		writeErr(c, 400, "invalid_id", "任务编号无效")
		return
	}
	key := operationID("review", body.OperationKey)
	store := s.app.Runtime.(workbenchCommands)
	respond := func(d domain.ReviewDecision) {
		c.JSON(201, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]any{"decision": losslessJSON(d), "requires_explicit_resume": true}})
	}
	previous, err := store.WorkbenchReview(c.Request.Context(), id, domain.ReviewDecisionID(key))
	if err == nil {
		if previous.ExpectedRunVersion != body.ExpectedVersion || previous.WorkflowRevision != body.WorkflowRevision || previous.Kind != body.Kind || previous.Reviewer != strings.TrimSpace(body.Reviewer) || previous.Reason != strings.TrimSpace(body.Reason) || previous.BudgetIncrease != increase {
			writeErr(c, 409, "idempotency_conflict", "操作身份已用于不同评审")
			return
		}
		respond(previous)
		return
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		writeError(c, err)
		return
	}
	guard, err := s.app.Locks.TryAcquireRun(id, runlock.Exclusive)
	if err != nil {
		writeApplicationError(c, err)
		return
	}
	defer guard.Close()
	snapshot, err := s.app.Runtime.GetRun(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	if snapshot.Version != body.ExpectedVersion || snapshot.WorkflowRevision != body.WorkflowRevision {
		writeErr(c, 409, "version_conflict", "任务已更新，请核对最新版本")
		return
	}
	if snapshot.State != domain.RunNeedsReview {
		writeErr(c, 409, "invalid_state", "任务当前不需要评审")
		return
	}
	input, evidence, policy, err := store.WorkbenchReviewBinding(c.Request.Context(), id, snapshot.CurrentStage)
	if err != nil {
		writeError(c, err)
		return
	}
	req := domain.CreateReviewRequest{ID: domain.ReviewDecisionID(key), RunID: id, ExpectedRunVersion: body.ExpectedVersion, WorkflowRevision: body.WorkflowRevision, StageName: snapshot.CurrentStage, StageInputDigest: input, EvidenceDigest: evidence, PolicyDigest: policy, Kind: body.Kind, BudgetIncrease: increase, Reviewer: strings.TrimSpace(body.Reviewer), Reason: strings.TrimSpace(body.Reason), IdempotencyKey: key, At: time.Now().UTC()}
	if err = req.Validate(); err != nil {
		writeErr(c, 422, "invalid_review_payload", "重试需增加预算，拒绝需完整理由")
		return
	}
	d, err := s.app.Reviews.CreateReview(c.Request.Context(), req)
	if err != nil {
		writeApplicationError(c, err)
		return
	}
	respond(d)
}

// Kept separate from admission: bootstrapping providers is per task and uses
// the immutable effective configuration recorded with that run.
func (s *Server) executionApp(ctx context.Context, id domain.RunID) (*application.Application, error) {
	reader := s.app.Runtime.(interface {
		GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
		RunViewDocuments(context.Context, domain.RunID) ([]byte, []byte, error)
	})
	frozen, err := application.ConfigForRun(ctx, s.cfg, id, reader)
	if err != nil {
		return nil, err
	}
	return application.Bootstrap(ctx, frozen)
}
func (s *Server) executeExpected(ctx context.Context, id domain.RunID, version *int64) error {
	app, err := s.executionApp(ctx, id)
	if err != nil {
		return err
	}
	defer app.Close()
	runs := app.Runs.(*application.LocalRunService)
	if version != nil {
		_, err = runs.ResumeExpectedImmediate(ctx, id, *version)
	} else {
		_, err = runs.ResumeImmediate(ctx, id)
	}
	return err
}
