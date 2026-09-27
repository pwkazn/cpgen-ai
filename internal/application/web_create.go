package application

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

var ErrCreateIdempotencyConflict = errors.New("create identity was already used for a different request")

func webCreateRunID(operationKey string) domain.RunID {
	digest := domain.SumBytes([]byte("cpgen.web.create/v1:" + operationKey))
	return domain.RunID("run_" + string(digest[len("sha256:"):len("sha256:")+32]))
}

func webRequestDigest(cfg config.Config, request domain.RunRequest) (domain.Digest, error) {
	revision := workflow.FakeRevision
	if cfg.Workflow != nil {
		revision = cfg.Workflow.Revision
	}
	definition, err := workflow.DefinitionFor(revision)
	if err != nil {
		return "", err
	}
	if definition.UsesGeneration() {
		submitted, err := domain.GenerationRequestFromRunRequest(request)
		if err != nil {
			return "", err
		}
		return submitted.Digest()
	}
	raw, err := canonicalJSON(request)
	if err != nil {
		return "", err
	}
	return domain.SumBytes(raw), nil
}

// LookupCreate resolves a durable create identity before capacity admission.
// A retry can read its original frozen run even while all execution slots are busy.
func LookupCreate(ctx context.Context, app *Application, cfg config.Config, request domain.RunRequest, operationKey string) (domain.RunSnapshot, bool, error) {
	if app == nil || app.Runtime == nil || strings.TrimSpace(operationKey) == "" || len(operationKey) > 256 {
		return domain.RunSnapshot{}, false, errors.New("invalid create lookup")
	}
	if err := request.Validate(); err != nil {
		return domain.RunSnapshot{}, false, err
	}
	digest, err := webRequestDigest(cfg, request)
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	snapshot, err := app.Runtime.GetRun(ctx, webCreateRunID(operationKey))
	if errors.Is(err, sqlite.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return domain.RunSnapshot{}, false, nil
	}
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	if snapshot.RequestDigest != digest {
		return domain.RunSnapshot{}, false, ErrCreateIdempotencyConflict
	}
	return snapshot, true, nil
}

// CreateOnly persists a CREATED run through local storage without constructing
// provider/Docker execution resources and without starting a workflow stage.
func CreateOnly(ctx context.Context, app *Application, cfg config.Config, request domain.RunRequest, operationKey string) (domain.RunSnapshot, bool, error) {
	if ctx == nil || app == nil || app.Runtime == nil || app.Locks == nil {
		return domain.RunSnapshot{}, false, errors.New("create requires context and local application storage")
	}
	if err := ctx.Err(); err != nil {
		return domain.RunSnapshot{}, false, err
	}
	if strings.TrimSpace(operationKey) == "" || len(operationKey) > 256 {
		return domain.RunSnapshot{}, false, errors.New("create operation key is required")
	}
	if err := request.Validate(); err != nil {
		return domain.RunSnapshot{}, false, err
	}
	cfg, effectiveJSON, err := FreezeEffectiveConfig(cfg)
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	revision := workflow.FakeRevision
	if cfg.Workflow != nil {
		revision = cfg.Workflow.Revision
	}
	definition, err := workflow.DefinitionFor(revision)
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	runID := webCreateRunID(operationKey)
	var create domain.CreateRunRequest
	if definition.UsesGeneration() {
		submitted, err := domain.GenerationRequestFromRunRequest(request)
		if err != nil {
			return domain.RunSnapshot{}, false, err
		}
		seed := int64(0)
		if submitted.Seed != nil {
			seed = *submitted.Seed
		} else {
			var raw [8]byte
			if _, err := rand.Read(raw[:]); err != nil {
				return domain.RunSnapshot{}, false, err
			}
			seed = int64(binary.LittleEndian.Uint64(raw[:]))
		}
		input, err := domain.NewGenerationRequestSnapshotV1(submitted, seed)
		if err != nil {
			return domain.RunSnapshot{}, false, err
		}
		raw, err := submitted.CanonicalJSON()
		if err != nil {
			return domain.RunSnapshot{}, false, err
		}
		create = domain.CreateRunRequest{RunID: runID, SubmittedRequestJSON: raw, SubmittedRequestDigest: input.RequestDigest, EffectiveSeed: seed, RedactedEffectiveConfigJSON: effectiveJSON, RedactedEffectiveConfigDigest: domain.SumBytes(effectiveJSON), WorkflowRevision: revision, WorkflowDigest: domain.SumBytes([]byte(revision)), SchemaVersion: domain.RequestSchemaV1, BudgetLimits: submitted.BudgetLimits, StageSequence: definition.Stages(), CreatedAt: time.Now().UTC(), IdempotencyKey: stableServiceID("web-create", runID, 1)}
	} else {
		raw, err := canonicalJSON(request)
		if err != nil {
			return domain.RunSnapshot{}, false, err
		}
		seed := int64(0)
		if request.Seed != nil {
			seed = *request.Seed
		} else {
			seed = int64(len(request.Brief))
		}
		create = domain.CreateRunRequest{RunID: runID, SubmittedRequestJSON: raw, SubmittedRequestDigest: domain.SumBytes(raw), EffectiveSeed: seed, RedactedEffectiveConfigJSON: effectiveJSON, RedactedEffectiveConfigDigest: domain.SumBytes(effectiveJSON), WorkflowRevision: revision, SchemaVersion: domain.SchemaVersion(request.SchemaVersion), WorkflowDigest: domain.SumBytes([]byte(revision)), BudgetLimits: request.BudgetLimits, StageSequence: definition.Stages(), CreatedAt: time.Now().UTC(), IdempotencyKey: stableServiceID("web-create", runID, 1)}
	}
	if err := create.Validate(); err != nil {
		return domain.RunSnapshot{}, false, err
	}
	if existing, readErr := app.Runtime.GetRun(ctx, runID); readErr == nil {
		if existing.RequestDigest == create.SubmittedRequestDigest {
			return existing, false, nil
		}
		return domain.RunSnapshot{}, false, ErrCreateIdempotencyConflict
	}
	guard, err := app.Locks.TryAcquireRun(runID, runlock.Exclusive)
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	defer guard.Close()
	artifactGuard, err := app.Locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	defer artifactGuard.Close()
	snapshot, err := app.Runtime.CreateRun(ctx, create)
	if err == nil {
		return snapshot, true, nil
	}
	existing, readErr := app.Runtime.GetRun(ctx, runID)
	if readErr == nil {
		if existing.RequestDigest == create.SubmittedRequestDigest {
			return existing, false, nil
		}
		return domain.RunSnapshot{}, false, ErrCreateIdempotencyConflict
	}
	return domain.RunSnapshot{}, false, err
}
