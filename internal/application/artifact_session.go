package application

import (
	"cpgen/internal/adapter/storage/blob"
	artifactsession "cpgen/internal/artifact"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// PreparedArtifactSession remains part of the application API for source
// compatibility. The implementation lives in the lower-level artifact
// package, keeping storage adapters independent of application composition.
type PreparedArtifactSession = artifactsession.PreparedArtifactSession

func NewPreparedArtifactSession(ledger port.ArtifactLedger, store *blob.Store, prepared domain.PreparedCalls) (PreparedArtifactSession, error) {
	return artifactsession.NewPreparedArtifactSession(ledger, store, prepared)
}
