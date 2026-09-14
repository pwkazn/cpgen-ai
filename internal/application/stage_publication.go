package application

import (
	"context"
	"errors"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
)

type StageArtifactPublisher interface {
	RunID() domain.RunID
	Publish(context.Context, port.ArtifactDeclaration, []byte) (domain.PendingArtifact, error)
}

type StagePublisher func(domain.StageAttempt, int64) (StageArtifactPublisher, error)

func newStagePublisher(store durable.Store, blobs *blob.Store, source clock.Clock) StagePublisher {
	return func(attempt domain.StageAttempt, version int64) (StageArtifactPublisher, error) {
		identity, err := stagePublicationIdentity(attempt, version)
		if err != nil {
			return nil, err
		}
		return sandboxexec.NewArtifactSink(store, blobs, source, identity)
	}
}

// Adapt to the existing persisted protocol without changing any canonical
// bytes or recovery IDs. A separate publication ledger would require migration.
func stagePublicationIdentity(a domain.StageAttempt, version int64) (port.SandboxAuthorizationIdentity, error) {
	if err := a.Validate(); err != nil {
		return port.SandboxAuthorizationIdentity{}, err
	}
	if a.State != domain.StageAttemptRunning || version <= 0 {
		return port.SandboxAuthorizationIdentity{}, errors.New("publication requires an admitted running attempt")
	}
	switch a.StageName {
	case "solution_verify":
		return solutionVerificationIdentity(a, version), nil
	case "data_verify":
		return dataVerificationIdentity(a, version, false), nil
	case "judge":
		return judgeVerificationIdentity(a, version), nil
	case "quality":
		return qualityVerificationIdentity(a, version), nil
	case "package":
		return packagePublicationIdentity(a, version), nil
	default:
		return port.SandboxAuthorizationIdentity{}, errors.New("stage has no publication protocol")
	}
}

func packagePublicationIdentity(attempt domain.StageAttempt, version int64) port.SandboxAuthorizationIdentity {
	const operation = "package-publication"
	return port.SandboxAuthorizationIdentity{RunID: attempt.RunID, StageName: attempt.StageName, AttemptID: attempt.AttemptID, SandboxExecutionID: domain.SandboxExecutionID(durable.MutationID("sandbox", operation, attempt.RunID, attempt.AttemptID, attempt.InputDigest)), LogicalOperationID: operation, Kind: domain.CallSandboxCompile, ScopeDigest: attempt.InputDigest, ExpectedRunVersion: version}
}
