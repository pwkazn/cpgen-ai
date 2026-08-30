package port

import (
	"context"
	"fmt"
	"io"

	"cpgen/internal/domain"
)

type ArtifactDeclaration struct {
	MediaType   string                     `json:"media_type"`
	Role        domain.ArtifactRole        `json:"role"`
	LogicalPath domain.SafeRelPath         `json:"logical_path"`
	MaxBytes    int64                      `json:"max_bytes"`
	Provenance  domain.ProvenanceCandidate `json:"provenance"`
}

func (d ArtifactDeclaration) Validate() error {
	if d.MediaType == "" {
		return fmt.Errorf("artifact media type is required")
	}
	if !d.Role.Valid() {
		return fmt.Errorf("invalid artifact role %q", d.Role)
	}
	if err := d.LogicalPath.Validate(); err != nil {
		return err
	}
	if d.MaxBytes <= 0 {
		return fmt.Errorf("artifact max bytes must be positive")
	}
	return d.Provenance.Validate()
}

type ArtifactWriter interface {
	io.Writer
	Finalize(ctx context.Context) (domain.PendingArtifact, error)
	Abort(ctx context.Context) error
}

type MeteredArtifactSink interface {
	Prepare(ctx context.Context, declaration ArtifactDeclaration) (ArtifactWriter, error)
	PinExisting(ctx context.Context, blob domain.BlobRef, declaration ArtifactDeclaration) (domain.PendingArtifact, error)
}

type VerifiedReadCloser interface {
	io.ReadCloser
	BlobRef() domain.BlobRef
}

type VerifiedBlobReader interface {
	OpenVerified(ctx context.Context, blob domain.BlobRef) (VerifiedReadCloser, error)
}
