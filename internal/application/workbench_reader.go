package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const DefaultWorkbenchArtifactLimit int64 = 2 << 20

type WorkbenchStage struct {
	Name     domain.StageName      `json:"name"`
	Ordinal  int                   `json:"ordinal"`
	State    domain.StageState     `json:"state"`
	Attempts []domain.StageAttempt `json:"attempts"`
}

type WorkbenchArtifactRef struct {
	OccurrenceID domain.ArtifactOccurrenceID `json:"occurrence_id"`
	Digest       domain.Digest               `json:"digest"`
	Size         int64                       `json:"size"`
	Role         domain.ArtifactRole         `json:"role"`
	LogicalPath  domain.SafeRelPath          `json:"logical_path"`
	StageName    domain.StageName            `json:"stage_name"`
	MediaType    string                      `json:"media_type"`
}

type WorkbenchRunDetail struct {
	Run            domain.RunSnapshot               `json:"run"`
	RequestJSON    []byte                           `json:"request_json"`
	ConfigJSON     []byte                           `json:"config_json"`
	Budget         domain.BudgetSnapshot            `json:"budget"`
	BudgetUsed     map[domain.BudgetDimension]int64 `json:"budget_used"`
	BudgetReserved map[domain.BudgetDimension]int64 `json:"budget_reserved"`
	Stages         []WorkbenchStage                 `json:"stages"`
	PendingReview  *domain.ReviewDecision           `json:"pending_review,omitempty"`
	PendingCancel  *domain.ControlRequest           `json:"pending_cancel,omitempty"`
	Artifacts      []WorkbenchArtifactRef           `json:"artifacts"`
	RecentEvents   []domain.RunEvent                `json:"recent_events"`
	BeforeVersion  int64                            `json:"before_version"`
}

type WorkbenchArtifactContent struct {
	Ref     WorkbenchArtifactRef `json:"artifact"`
	Content []byte               `json:"content"`
}

// WorkbenchReader is a read-only application capability; callers never receive
// a filesystem path or an unverified blob handle.
type WorkbenchReader struct {
	store port.WorkbenchRunReader
	blobs port.VerifiedBlobReader
}

func NewWorkbenchReader(store port.WorkbenchRunReader, blobs port.VerifiedBlobReader) (*WorkbenchReader, error) {
	if store == nil || blobs == nil {
		return nil, errors.New("workbench reader requires local verified storage")
	}
	return &WorkbenchReader{store: store, blobs: blobs}, nil
}

func (r *WorkbenchReader) ReadRunDetail(ctx context.Context, runID domain.RunID) (WorkbenchRunDetail, error) {
	var result WorkbenchRunDetail
	if ctx == nil {
		return result, errors.New("workbench read requires a context")
	}
	snapshot, err := r.store.ReadWorkbenchRun(ctx, runID)
	if err != nil {
		return result, err
	}
	result = WorkbenchRunDetail{Run: snapshot.Run, RequestJSON: append([]byte(nil), snapshot.RequestJSON...), ConfigJSON: append([]byte(nil), snapshot.ConfigJSON...), Budget: snapshot.Budget, BudgetUsed: snapshot.BudgetUsed, BudgetReserved: snapshot.BudgetReserved, PendingReview: snapshot.PendingReview, RecentEvents: snapshot.RecentEvents, BeforeVersion: snapshot.BeforeVersion}
	for _, stage := range snapshot.Stages {
		result.Stages = append(result.Stages, WorkbenchStage{Name: stage.Name, Ordinal: stage.Ordinal, State: stage.State, Attempts: stage.Attempts})
	}
	seenDrafts := map[domain.StageName]bool{}
	result.PendingCancel = snapshot.PendingCancel
	for _, artifact := range snapshot.Artifacts {
		if workbenchDraft(artifact) {
			if seenDrafts[artifact.StageName] {
				continue
			}
			seenDrafts[artifact.StageName] = true
		}
		if workbenchContentAllowed(artifact) || workbenchDraft(artifact) {
			result.Artifacts = append(result.Artifacts, workbenchArtifactRef(artifact))
		}
	}
	return result, nil
}

func (r *WorkbenchReader) ReadArtifact(ctx context.Context, runID domain.RunID, occurrenceID domain.ArtifactOccurrenceID, maxBytes int64) (WorkbenchArtifactContent, error) {
	var result WorkbenchArtifactContent
	if ctx == nil {
		return result, errors.New("workbench artifact read requires a context")
	}
	if maxBytes <= 0 || maxBytes > DefaultWorkbenchArtifactLimit {
		maxBytes = DefaultWorkbenchArtifactLimit
	}
	artifact, err := r.store.ReadWorkbenchArtifact(ctx, runID, occurrenceID)
	if err != nil {
		return result, err
	}
	if workbenchDraft(artifact) {
		data, err := r.readDraftContent(ctx, runID, artifact)
		if err != nil {
			return result, err
		}
		if int64(len(data)) > maxBytes {
			return result, errors.New("draft exceeds preview limit")
		}
		return WorkbenchArtifactContent{Ref: workbenchArtifactRef(artifact), Content: data}, nil
	}
	if !workbenchContentAllowed(artifact) {
		return result, errors.New("artifact is not an approved workbench content type")
	}
	if evidence, ok := r.store.(SandboxEvidenceReadStore); ok {
		stage, err := evidence.ReadCommittedSandboxStage(ctx, runID, artifact.StageName)
		if err != nil {
			return result, err
		}
		found := false
		for _, item := range stage.Artifacts {
			if item.OccurrenceID == artifact.OccurrenceID && item.Blob.Blob == artifact.Blob {
				found = true
			}
		}
		if !found {
			return result, errors.New("artifact does not belong to the current proven stage")
		}
	} else {
		return result, errors.New("artifact provenance reader unavailable")
	}
	if artifact.Blob.Size > maxBytes {
		return result, fmt.Errorf("artifact exceeds workbench preview limit of %d bytes", maxBytes)
	}
	reader, err := r.blobs.OpenVerified(ctx, artifact.Blob)
	if err != nil {
		return result, err
	}
	defer reader.Close()
	if reader.BlobRef() != artifact.Blob {
		return result, errors.New("verified artifact reference differs from committed occurrence")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return result, err
	}
	if int64(len(data)) > maxBytes {
		return result, fmt.Errorf("artifact exceeds workbench preview limit of %d bytes", maxBytes)
	}
	result = WorkbenchArtifactContent{Ref: workbenchArtifactRef(artifact), Content: data}
	return result, nil
}

func workbenchArtifactRef(a port.WorkbenchArtifactSnapshot) WorkbenchArtifactRef {
	ref := WorkbenchArtifactRef{OccurrenceID: a.OccurrenceID, Digest: a.Blob.Digest, Size: a.Blob.Size, Role: a.Role, LogicalPath: a.LogicalPath, StageName: a.StageName, MediaType: a.MediaType}
	if workbenchDraft(a) {
		ref.LogicalPath = domain.SafeRelPath(string(a.StageName) + "/verified-draft.md")
		ref.MediaType = "text/markdown"
	}
	return ref
}

func workbenchDraft(a port.WorkbenchArtifactSnapshot) bool {
	return a.MediaType == "application/vnd.cpgen.llm-response+json" && (a.StageName == "statement" || a.StageName == "solution")
}

func workbenchContentAllowed(a port.WorkbenchArtifactSnapshot) bool {
	if a.Blob.Size < 0 || a.MediaType == "" {
		return false
	}
	media := strings.ToLower(strings.TrimSpace(strings.Split(a.MediaType, ";")[0]))
	textual := strings.HasPrefix(media, "text/") || media == "application/json" || media == "application/yaml" || media == "application/x-yaml"
	if !textual {
		return false
	}
	path := strings.ToLower(string(a.LogicalPath))
	if strings.HasPrefix(path, "private/") {
		return false
	}
	roleAllowed := a.Role == domain.ArtifactSource || a.Role == domain.ArtifactProgram || a.Role == domain.ArtifactEvidence
	pathAllowed := strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".txt") || strings.HasSuffix(path, ".cpp") || strings.HasSuffix(path, ".h") || strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")
	return roleAllowed && pathAllowed
}
