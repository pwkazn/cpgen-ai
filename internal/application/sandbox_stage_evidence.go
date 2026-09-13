package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	docker "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type sandboxEvidenceStore interface {
	port.SandboxLifecycleReader
	ReadLogicalCall(context.Context, domain.CallRecordID) (domain.CallRecord, error)
}

// Shared read-only proof checks for stages consuming actual sandbox receipts.
// It cannot create a writer, ledger grant, container, or recovery execution.
type sandboxStageEvidence struct {
	ctx   context.Context
	store sandboxEvidenceStore
	blobs port.VerifiedBlobReader
	items map[domain.SafeRelPath]port.CommittedPrivateStageArtifact
	used  map[domain.SafeRelPath]bool
}

func newSandboxStageEvidence(ctx context.Context, store sandboxEvidenceStore, blobs port.VerifiedBlobReader, stage port.CommittedPrivateStage) (*sandboxStageEvidence, error) {
	reader := &sandboxStageEvidence{ctx, store, blobs, make(map[domain.SafeRelPath]port.CommittedPrivateStageArtifact), make(map[domain.SafeRelPath]bool)}
	for _, item := range stage.Artifacts {
		if _, exists := reader.items[item.Blob.LogicalPath]; exists {
			return nil, errors.New("sandbox stage repeats an artifact path")
		}
		reader.items[item.Blob.LogicalPath] = item
	}
	return reader, nil
}

func (r *sandboxStageEvidence) read(path domain.SafeRelPath, ref domain.BlobRef, role domain.ArtifactRole, limit int64) ([]byte, error) {
	item, found := r.items[path]
	if !found || item.Blob.Blob != ref || item.Blob.Role != role {
		return nil, fmt.Errorf("sandbox stage artifact is absent or differs: %s", path)
	}
	raw, err := readSolutionVerificationBlob(r.ctx, r.blobs, ref, limit)
	if err == nil {
		r.used[path] = true
	}
	return raw, err
}

func (r *sandboxStageEvidence) pending(p *domain.PendingArtifact) error {
	if p == nil {
		return nil
	}
	item, found := r.items[p.LogicalPath]
	if !found || item.Blob.MediaType != p.MediaType || !reflect.DeepEqual(item.Blob.Provenance, p.Provenance) {
		return errors.New("sandbox process artifact metadata differs")
	}
	call, err := r.store.ReadLogicalCall(r.ctx, item.CurrentCallRecordID)
	if err != nil {
		return err
	}
	if call.ResultAttemptCallID == nil || *call.ResultAttemptCallID != p.CallID {
		return errors.New("sandbox artifact changed its local producer")
	}
	_, err = r.read(p.LogicalPath, p.Blob, p.Role, 8<<20)
	return err
}

func (r *sandboxStageEvidence) result(config SandboxReadPolicy, request, result any) error {
	var kind domain.CallKind
	var pending []*domain.PendingArtifact
	switch request.(type) {
	case port.CompileRequest:
		kind = domain.CallSandboxCompile
		value, ok := result.(port.CompileResult)
		if !ok {
			return errors.New("compile evidence has another result type")
		}
		pending = []*domain.PendingArtifact{value.Program, value.Stdout, value.Stderr, value.Execution}
	case port.RunRequest:
		kind = domain.CallSandboxRun
		value, ok := result.(port.RunResult)
		if !ok {
			return errors.New("run evidence has another result type")
		}
		pending = []*domain.PendingArtifact{value.Stdout, value.Stderr, value.Execution}
		for _, item := range value.Outputs {
			p := item
			pending = append(pending, &p)
		}
	default:
		return errors.New("unsupported sandbox evidence request")
	}
	identity, planIdentity, err := sandboxReadOperationIdentity(config, kind, request)
	if err != nil {
		return err
	}
	var plan port.ContainerPlan
	switch request := request.(type) {
	case port.CompileRequest:
		plan, err = docker.BuildCompilePlan(request, config.Lock, planIdentity)
	case port.RunRequest:
		plan, err = docker.BuildRunPlan(request, config.Lock, planIdentity)
	}
	if err != nil {
		return err
	}
	path := domain.SafeRelPath("sandbox/" + string(identity.SandboxExecutionID) + "/result.json")
	item, found := r.items[path]
	if !found || item.Blob.MediaType != "application/vnd.cpgen.sandbox-result+json" || item.Blob.Provenance.SchemaVersion != "cpgen.sandbox-result/v1" || item.Blob.Provenance.Producer != "docker" || item.Blob.Provenance.InputDigest == nil || *item.Blob.Provenance.InputDigest != identity.ScopeDigest {
		return errors.New("sandbox stage lacks its exact request receipt")
	}
	raw, err := r.read(path, item.Blob.Blob, domain.ArtifactEvidence, 1<<20)
	if err != nil {
		return err
	}
	var receipt sandboxResultReceipt[json.RawMessage]
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return err
	}
	expected, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if receipt.Schema != "cpgen.sandbox-result/v1" || receipt.Scope != identity.ScopeDigest || receipt.Plan != plan.PlanDigest || !bytes.Equal(receipt.Result, expected) {
		return errors.New("sandbox result differs from its exact request receipt")
	}
	if err := verifySandboxCleaned(r.ctx, r.store, identity, plan); err != nil {
		return err
	}
	for _, item := range pending {
		if err := r.pending(item); err != nil {
			return err
		}
	}
	return nil
}

func (r *sandboxStageEvidence) complete() error {
	if len(r.used) != len(r.items) {
		return errors.New("sandbox stage contains unrelated artifacts")
	}
	return nil
}
