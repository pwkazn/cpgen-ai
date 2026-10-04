package web

import (
	"bytes"
	"cpgen/internal/domain"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"strconv"
	"time"
)

// losslessJSON keeps every persisted integer exact across the JavaScript boundary.
func losslessJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var result any
	if d.Decode(&result) != nil {
		return nil
	}
	return stringifyNumbers(result)
}
func stringifyNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		return x.String()
	case []any:
		for i := range x {
			x[i] = stringifyNumbers(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = stringifyNumbers(x[k])
		}
	}
	return v
}
func (s *Server) detail(c *gin.Context) {
	id := domain.RunID(c.Param("id"))
	if id.Validate() != nil {
		writeErr(c, 400, "invalid_id", "任务编号无效")
		return
	}
	detail, err := s.app.WebReads.ReadRunDetail(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	r := detail.Run
	observation := s.manager.observe(id)
	// The OS lock also observes executors owned by a CLI or another server.
	active := observation.Active
	if !active {
		guard, err := s.app.Locks.TryAcquireRun(id, runlock.Exclusive)
		if err == nil {
			_ = guard.Close()
		} else {
			active = true
		}
	}
	events := make([]eventDTO, 0, len(detail.RecentEvents))
	for _, e := range detail.RecentEvents {
		events = append(events, toEventDTO(e))
	}
	run := losslessJSON(toRunDTO(r)).(map[string]any)
	var request any
	decoder := json.NewDecoder(bytes.NewReader(detail.RequestJSON))
	decoder.UseNumber()
	if decoder.Decode(&request) == nil {
		request = stringifyNumbers(request)
		if m, ok := request.(map[string]any); ok {
			run["brief"] = m["brief"]
		}
	}
	artifacts := losslessJSON(detail.Artifacts)
	if artifacts == nil {
		artifacts = []any{}
	}
	data := map[string]any{"run": run, "request": request, "stages": losslessJSON(detail.Stages), "budget": losslessJSON(detail.Budget), "budget_used": losslessJSON(detail.BudgetUsed), "budget_reserved": losslessJSON(detail.BudgetReserved), "review": losslessJSON(detail.PendingReview), "pending_cancel": losslessJSON(detail.PendingCancel), "recent_events": events, "before_version": strconv.FormatInt(detail.BeforeVersion, 10), "artifacts": artifacts,
		"revision_targets":  revisionTargets(r),
		"execution":         map[string]any{"observed_at": time.Now().UTC(), "active": active, "last_error": observation.LastError},
		"available_actions": map[string]bool{"resume": !active && (r.State == domain.RunCreated || r.State == domain.RunRunning || r.State == domain.RunBlocked || (r.State == domain.RunNeedsReview && (detail.PendingReview != nil || detail.PendingCancel != nil))), "cancel": detail.PendingCancel == nil && r.State != domain.RunReady && r.State != domain.RunFailed && r.State != domain.RunCancelled, "review": detail.PendingCancel == nil && !active && r.State == domain.RunNeedsReview && detail.PendingReview == nil}}
	c.JSON(200, envelope{SchemaVersion: "cpgen.web/v1", Data: data})
}

func revisionTargets(run domain.RunSnapshot) []domain.StageName {
	definition, err := workflow.DefinitionFor(run.WorkflowRevision)
	if err != nil {
		return []domain.StageName{}
	}
	return definition.ManualRevisionTargets(run.CurrentStage)
}

func (s *Server) artifact(c *gin.Context) {
	id, occ := domain.RunID(c.Param("id")), domain.ArtifactOccurrenceID(c.Param("occurrence"))
	if id.Validate() != nil || occ.Validate() != nil {
		writeErr(c, 400, "invalid_id", "制品编号无效")
		return
	}
	runGuard, err := s.app.Locks.TryAcquireRun(id, runlock.Shared)
	if err != nil {
		writeApplicationError(c, err)
		return
	}
	defer runGuard.Close()
	guard, err := s.app.Locks.AcquireArtifacts(c.Request.Context(), runlock.Shared)
	if err != nil {
		writeError(c, err)
		return
	}
	defer guard.Close()
	content, err := s.app.WebReads.ReadArtifact(c.Request.Context(), id, occ, 0)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, envelope{SchemaVersion: "cpgen.web/v1", Data: map[string]any{"artifact": losslessJSON(content.Ref), "text": string(content.Content)}})
}
