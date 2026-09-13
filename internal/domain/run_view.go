package domain

import (
	"errors"
	"fmt"
	"slices"
)

// FakeInput and the following values support deterministic local exercises
// of execution, cancellation, and dependency recovery.
type FakeInput struct {
	Brief         string            `json:"brief"`
	RequestDigest Digest            `json:"request_digest"`
	ConfigDigest  Digest            `json:"config_digest"`
	Scenario      string            `json:"scenario,omitempty"`
	Payload       []byte            `json:"payload,omitempty"`
	Flags         map[string]string `json:"flags,omitempty"`
}

func (v FakeInput) Validate() error {
	if v.Brief == "" {
		return errors.New("fake input brief is required")
	}
	if err := v.RequestDigest.Validate(); err != nil {
		return err
	}
	if err := v.ConfigDigest.Validate(); err != nil {
		return err
	}
	return nil
}

type FakePrepared struct {
	Digest    Digest                 `json:"digest"`
	Summary   string                 `json:"summary"`
	Scenario  string                 `json:"scenario,omitempty"`
	Artifacts []CommittedArtifactRef `json:"artifacts,omitempty"`
}

func (v FakePrepared) Validate() error {
	if err := v.Digest.Validate(); err != nil {
		return err
	}
	if v.Summary == "" {
		return errors.New("fake prepared summary is required")
	}
	for index, item := range v.Artifacts {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("prepared artifact %d: %w", index, err)
		}
	}
	return nil
}

type FakeEvidence struct {
	Digest           Digest `json:"digest"`
	PreparedDigest   Digest `json:"prepared_digest"`
	DependencyID     string `json:"dependency_id,omitempty"`
	DependencyDigest Digest `json:"dependency_digest,omitempty"`
	PolicyDigest     Digest `json:"policy_digest,omitempty"`
	Scenario         string `json:"scenario,omitempty"`
}

func (v FakeEvidence) Validate() error {
	if err := v.Digest.Validate(); err != nil {
		return err
	}
	if err := v.PreparedDigest.Validate(); err != nil {
		return err
	}
	for name, digest := range map[string]Digest{"dependency": v.DependencyDigest, "policy": v.PolicyDigest} {
		if digest != "" {
			if err := digest.Validate(); err != nil {
				return fmt.Errorf("%s digest: %w", name, err)
			}
		}
	}
	if v.DependencyDigest != "" && v.DependencyID == "" {
		return errors.New("dependency id is required with dependency digest")
	}
	return nil
}

type FakeCheckpoint struct {
	Digest           Digest `json:"digest"`
	EvidenceDigest   Digest `json:"evidence_digest"`
	DependencyID     string `json:"dependency_id,omitempty"`
	DependencyDigest Digest `json:"dependency_digest,omitempty"`
	PolicyDigest     Digest `json:"policy_digest,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

func (v FakeCheckpoint) Validate() error {
	if err := v.Digest.Validate(); err != nil {
		return err
	}
	if err := v.EvidenceDigest.Validate(); err != nil {
		return err
	}
	for name, digest := range map[string]Digest{"dependency": v.DependencyDigest, "policy": v.PolicyDigest} {
		if digest != "" {
			if err := digest.Validate(); err != nil {
				return fmt.Errorf("%s digest: %w", name, err)
			}
		}
	}
	if v.DependencyDigest != "" && v.DependencyID == "" {
		return errors.New("dependency id is required with dependency digest")
	}
	return nil
}

// BudgetSnapshot is the read-only budget projection made available to a step.
// Remaining is copied on ingress and egress so a step cannot mutate the
// coordinator's authoritative accounting state.
type BudgetSnapshot struct {
	Limits    BudgetLimits              `json:"limits"`
	Remaining map[BudgetDimension]int64 `json:"remaining"`
	Version   int64                     `json:"version"`
}

func (b BudgetSnapshot) clone() BudgetSnapshot {
	result := b
	result.Remaining = make(map[BudgetDimension]int64, len(b.Remaining))
	for key, value := range b.Remaining {
		result.Remaining[key] = value
	}
	return result
}

func (b BudgetSnapshot) Validate() error {
	if err := b.Limits.Validate(); err != nil {
		return err
	}
	if b.Version < 0 {
		return errors.New("budget snapshot version must not be negative")
	}
	for dimension, remaining := range b.Remaining {
		if !dimension.Valid() || remaining < 0 {
			return fmt.Errorf("invalid budget snapshot entry %q", dimension)
		}
	}
	return nil
}

// CommittedArtifactRef is the small immutable artifact reference visible to a
// step. It deliberately contains no writer or persistence capability.
type CommittedArtifactRef struct {
	OccurrenceID ArtifactOccurrenceID `json:"occurrence_id"`
	Blob         BlobRef              `json:"blob"`
	Role         ArtifactRole         `json:"role"`
	LogicalPath  SafeRelPath          `json:"logical_path"`
}

func (r CommittedArtifactRef) Validate() error {
	if err := r.OccurrenceID.Validate(); err != nil {
		return err
	}
	if err := r.Blob.Validate(); err != nil {
		return err
	}
	if !r.Role.Valid() {
		return fmt.Errorf("invalid committed artifact role %q", r.Role)
	}
	return r.LogicalPath.Validate()
}

// ArtifactReference is a short compatibility alias for callers that prefer a
// generic name for committed artifact references.
type ArtifactReference = CommittedArtifactRef

// RunViewData is the construction-only value used to make an immutable view.
type RunViewData struct {
	RunID              RunID
	AttemptID          AttemptID
	WorkflowRevision   string
	SchemaVersion      SchemaVersion
	RequestDigest      Digest
	ConfigDigest       Digest
	WorkflowDigest     Digest
	State              RunState
	CurrentStage       StageName
	Version            int64
	RequestJSON        []byte
	ConfigJSON         []byte
	Budget             BudgetSnapshot
	CommittedArtifacts []CommittedArtifactRef
}

// RunView is a snapshot. Its slices and maps are private and all accessors
// return copies, allowing a step to run without sharing mutable state.
type RunView struct {
	runID              RunID
	attemptID          AttemptID
	workflowRevision   string
	schemaVersion      SchemaVersion
	requestDigest      Digest
	configDigest       Digest
	workflowDigest     Digest
	state              RunState
	currentStage       StageName
	version            int64
	requestJSON        []byte
	configJSON         []byte
	budget             BudgetSnapshot
	committedArtifacts []CommittedArtifactRef
}

func NewRunView(data RunViewData) (RunView, error) {
	if err := data.RunID.Validate(); err != nil {
		return RunView{}, err
	}
	if data.AttemptID != "" {
		if err := data.AttemptID.Validate(); err != nil {
			return RunView{}, err
		}
	}
	if data.WorkflowRevision == "" {
		return RunView{}, errors.New("workflow revision is required")
	}
	if err := data.SchemaVersion.Validate(); err != nil {
		return RunView{}, err
	}
	for name, digest := range map[string]Digest{
		"request": data.RequestDigest, "config": data.ConfigDigest, "workflow": data.WorkflowDigest,
	} {
		if err := digest.Validate(); err != nil {
			return RunView{}, fmt.Errorf("%s digest: %w", name, err)
		}
	}
	if !data.State.Valid() || data.Version <= 0 {
		return RunView{}, errors.New("run view state or version is invalid")
	}
	if data.CurrentStage != "" {
		if err := data.CurrentStage.Validate(); err != nil {
			return RunView{}, err
		}
	}
	if err := data.Budget.Validate(); err != nil {
		return RunView{}, fmt.Errorf("budget snapshot: %w", err)
	}
	artifacts := slices.Clone(data.CommittedArtifacts)
	for index, artifact := range artifacts {
		if err := artifact.Validate(); err != nil {
			return RunView{}, fmt.Errorf("committed artifact %d: %w", index, err)
		}
	}
	return RunView{
		runID: data.RunID, attemptID: data.AttemptID, workflowRevision: data.WorkflowRevision, schemaVersion: data.SchemaVersion,
		requestDigest: data.RequestDigest, configDigest: data.ConfigDigest, workflowDigest: data.WorkflowDigest,
		state: data.State, currentStage: data.CurrentStage, version: data.Version,
		requestJSON: slices.Clone(data.RequestJSON), configJSON: slices.Clone(data.ConfigJSON),
		budget: data.Budget.clone(), committedArtifacts: artifacts,
	}, nil
}

func NewRunViewFromSnapshot(snapshot RunSnapshot, budget BudgetSnapshot, artifacts []CommittedArtifactRef) (RunView, error) {
	return NewRunView(RunViewData{
		RunID: snapshot.RunID, WorkflowRevision: snapshot.WorkflowRevision, SchemaVersion: snapshot.SchemaVersion,
		RequestDigest: snapshot.RequestDigest, ConfigDigest: snapshot.ConfigDigest, WorkflowDigest: snapshot.WorkflowDigest,
		State: snapshot.State, CurrentStage: snapshot.CurrentStage, Version: snapshot.Version,
		Budget: budget, CommittedArtifacts: artifacts,
	})
}

func (v RunView) RunID() RunID                 { return v.runID }
func (v RunView) AttemptID() AttemptID         { return v.attemptID }
func (v RunView) WorkflowRevision() string     { return v.workflowRevision }
func (v RunView) SchemaVersion() SchemaVersion { return v.schemaVersion }
func (v RunView) RequestDigest() Digest        { return v.requestDigest }
func (v RunView) ConfigDigest() Digest         { return v.configDigest }
func (v RunView) WorkflowDigest() Digest       { return v.workflowDigest }
func (v RunView) State() RunState              { return v.state }
func (v RunView) CurrentStage() StageName      { return v.currentStage }
func (v RunView) Version() int64               { return v.version }
func (v RunView) RequestJSON() []byte          { return slices.Clone(v.requestJSON) }
func (v RunView) ConfigJSON() []byte           { return slices.Clone(v.configJSON) }
func (v RunView) Budget() BudgetSnapshot       { return v.budget.clone() }
func (v RunView) CommittedArtifacts() []CommittedArtifactRef {
	return slices.Clone(v.committedArtifacts)
}
