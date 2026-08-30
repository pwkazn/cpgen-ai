package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

var ErrArtifactTooLarge = errors.New("artifact exceeds declared byte limit")

type ArtifactSink struct {
	mu    sync.Mutex
	blobs map[domain.Digest][]byte
}

func NewArtifactSink() *ArtifactSink {
	return &ArtifactSink{blobs: make(map[domain.Digest][]byte)}
}

func (s *ArtifactSink) Prepare(ctx context.Context, declaration port.ArtifactDeclaration) (port.ArtifactWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := declaration.Validate(); err != nil {
		return nil, err
	}
	return &artifactWriter{sink: s, declaration: declaration}, nil
}

func (s *ArtifactSink) PinExisting(ctx context.Context, blob domain.BlobRef, declaration port.ArtifactDeclaration) (domain.PendingArtifact, error) {
	if err := ctx.Err(); err != nil {
		return domain.PendingArtifact{}, err
	}
	if err := declaration.Validate(); err != nil {
		return domain.PendingArtifact{}, err
	}
	if err := blob.Validate(); err != nil {
		return domain.PendingArtifact{}, err
	}
	if blob.Size > declaration.MaxBytes {
		return domain.PendingArtifact{}, ErrArtifactTooLarge
	}
	s.mu.Lock()
	data, exists := s.blobs[blob.Digest]
	s.mu.Unlock()
	if !exists || int64(len(data)) != blob.Size || domain.SumBytes(data) != blob.Digest {
		return domain.PendingArtifact{}, fmt.Errorf("blob is missing or failed verification")
	}
	return newPendingArtifact(blob, 0, declaration)
}

func (s *ArtifactSink) OpenVerified(ctx context.Context, blob domain.BlobRef) (port.VerifiedReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := blob.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	data, exists := s.blobs[blob.Digest]
	copyData := append([]byte(nil), data...)
	s.mu.Unlock()
	if !exists || int64(len(copyData)) != blob.Size || domain.SumBytes(copyData) != blob.Digest {
		return nil, fmt.Errorf("blob is missing or failed verification")
	}
	return &verifiedReader{Reader: bytes.NewReader(copyData), blob: blob}, nil
}

type artifactWriter struct {
	mu          sync.Mutex
	sink        *ArtifactSink
	declaration port.ArtifactDeclaration
	buffer      bytes.Buffer
	terminal    bool
}

func (w *artifactWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminal {
		return 0, fmt.Errorf("artifact writer is already terminal")
	}
	if int64(w.buffer.Len()+len(data)) > w.declaration.MaxBytes {
		return 0, ErrArtifactTooLarge
	}
	return w.buffer.Write(data)
}

func (w *artifactWriter) Finalize(ctx context.Context) (domain.PendingArtifact, error) {
	if err := ctx.Err(); err != nil {
		return domain.PendingArtifact{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminal {
		return domain.PendingArtifact{}, fmt.Errorf("artifact writer is already terminal")
	}
	w.terminal = true
	data := append([]byte(nil), w.buffer.Bytes()...)
	blob := domain.BlobRef{Digest: domain.SumBytes(data), Size: int64(len(data))}
	w.sink.mu.Lock()
	_, existed := w.sink.blobs[blob.Digest]
	if !existed {
		w.sink.blobs[blob.Digest] = data
	}
	w.sink.mu.Unlock()
	physicalBytes := blob.Size
	if existed {
		physicalBytes = 0
	}
	return newPendingArtifact(blob, physicalBytes, w.declaration)
}

func (w *artifactWriter) Abort(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminal {
		return fmt.Errorf("artifact writer is already terminal")
	}
	w.terminal = true
	w.buffer.Reset()
	return nil
}

func newPendingArtifact(blob domain.BlobRef, physicalBytes int64, declaration port.ArtifactDeclaration) (domain.PendingArtifact, error) {
	callID, err := domain.NewID("call")
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	reservationID, err := domain.NewID("reservation")
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	tokenID, err := domain.NewID("writer")
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	pinID, err := domain.NewID("pin")
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	artifact := domain.PendingArtifact{
		Blob: blob, MediaType: declaration.MediaType, Role: declaration.Role,
		LogicalPath: declaration.LogicalPath, CallID: domain.AttemptCallID(callID),
		ReservationID: domain.ReservationID(reservationID), WriterTokenID: domain.ArtifactWriterTokenID(tokenID),
		PinID: domain.BlobPinID(pinID), PhysicalNewBytes: physicalBytes, Provenance: declaration.Provenance,
	}
	return artifact, artifact.Validate()
}

type verifiedReader struct {
	*bytes.Reader
	blob domain.BlobRef
}

func (r *verifiedReader) Close() error            { return nil }
func (r *verifiedReader) BlobRef() domain.BlobRef { return r.blob }

var _ io.ReadCloser = (*verifiedReader)(nil)
var _ port.MeteredArtifactSink = (*ArtifactSink)(nil)
var _ port.VerifiedBlobReader = (*ArtifactSink)(nil)
