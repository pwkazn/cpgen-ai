//go:build cpgen_slice0_probe

package probe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// MemoryArtifactStore is the bounded Slice 0 stand-in for the production
// artifact backend. It is intentionally available only with the probe build
// tag and never creates formal artifact occurrences or READY state.
type MemoryArtifactStore struct {
	mu      sync.Mutex
	blobs   map[domain.Digest][]byte
	callFor map[domain.ArtifactRole]domain.AttemptCallID
}

func NewMemoryArtifactStore() *MemoryArtifactStore {
	return &MemoryArtifactStore{blobs: map[domain.Digest][]byte{}, callFor: map[domain.ArtifactRole]domain.AttemptCallID{}}
}

func (s *MemoryArtifactStore) PutBlob(data []byte) domain.BlobRef {
	ref, _ := s.put(data)
	return ref
}

func (s *MemoryArtifactStore) put(data []byte) (domain.BlobRef, bool) {
	digest := domain.SumBytes(data)
	ref := domain.BlobRef{Digest: digest, Size: int64(len(data))}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.blobs[digest]
	if !existed {
		s.blobs[digest] = bytes.Clone(data)
	}
	return ref, !existed
}

func (s *MemoryArtifactStore) ReadBlob(ref domain.BlobRef) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	data, ok := s.blobs[ref.Digest]
	s.mu.Unlock()
	if !ok || int64(len(data)) != ref.Size || domain.SumBytes(data) != ref.Digest {
		return nil, fmt.Errorf("probe blob %s is missing or corrupt", ref.Digest)
	}
	return bytes.Clone(data), nil
}

func (s *MemoryArtifactStore) configureCalls(plan port.ContainerPlan, calls []domain.AttemptCallID) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	roles := map[port.ResourceRole]domain.AttemptCallID{}
	for _, resource := range plan.Resources {
		if resource.Kind != port.ResourceContainer {
			continue
		}
		if resource.CreateCallOrdinal == nil || *resource.CreateCallOrdinal >= len(calls) {
			return fmt.Errorf("probe artifact call mapping is incomplete")
		}
		roles[resource.Role] = calls[*resource.CreateCallOrdinal]
	}
	target := roles[port.ResourceTarget]
	export := roles[port.ResourceExport]
	if target == "" {
		return fmt.Errorf("probe artifact mapping has no target call")
	}
	if export == "" {
		export = target
	}
	callFor := map[domain.ArtifactRole]domain.AttemptCallID{
		domain.ArtifactProgram: target, domain.ArtifactOutput: export,
		domain.ArtifactStdout: target, domain.ArtifactStderr: target,
		domain.ArtifactCompileLog: target, domain.ArtifactExecutionLog: target, domain.ArtifactEvidence: target,
	}
	if roles[port.ResourceExport] != "" {
		callFor[domain.ArtifactProgram] = export
	}
	s.mu.Lock()
	s.callFor = callFor
	s.mu.Unlock()
	return nil
}

func (s *MemoryArtifactStore) configureProbeArtifactCall(callID domain.AttemptCallID) error {
	if err := callID.Validate(); err != nil {
		return err
	}
	callFor := map[domain.ArtifactRole]domain.AttemptCallID{}
	for _, role := range []domain.ArtifactRole{
		domain.ArtifactSource, domain.ArtifactProgram, domain.ArtifactInput, domain.ArtifactOutput,
		domain.ArtifactStdout, domain.ArtifactStderr, domain.ArtifactCompileLog,
		domain.ArtifactExecutionLog, domain.ArtifactEvidence,
	} {
		callFor[role] = callID
	}
	s.mu.Lock()
	s.callFor = callFor
	s.mu.Unlock()
	return nil
}

func (s *MemoryArtifactStore) OpenVerified(ctx context.Context, ref domain.BlobRef) (port.VerifiedReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := s.ReadBlob(ref)
	if err != nil {
		return nil, err
	}
	return &memoryVerifiedReader{Reader: bytes.NewReader(data), ref: ref}, nil
}

func (s *MemoryArtifactStore) Prepare(ctx context.Context, declaration port.ArtifactDeclaration) (port.ArtifactWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := declaration.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	callID := s.callFor[declaration.Role]
	s.mu.Unlock()
	if callID == "" {
		return nil, fmt.Errorf("probe artifact role %q has no predeclared producer call", declaration.Role)
	}
	return &memoryArtifactWriter{store: s, declaration: declaration, callID: callID}, nil
}

func (s *MemoryArtifactStore) PinExisting(ctx context.Context, blob domain.BlobRef, declaration port.ArtifactDeclaration) (domain.PendingArtifact, error) {
	if err := ctx.Err(); err != nil {
		return domain.PendingArtifact{}, err
	}
	if _, err := s.ReadBlob(blob); err != nil {
		return domain.PendingArtifact{}, err
	}
	if err := declaration.Validate(); err != nil {
		return domain.PendingArtifact{}, err
	}
	s.mu.Lock()
	callID := s.callFor[declaration.Role]
	s.mu.Unlock()
	if callID == "" {
		return domain.PendingArtifact{}, fmt.Errorf("probe artifact role %q has no predeclared producer call", declaration.Role)
	}
	return newPendingArtifact(blob, declaration, callID, 0)
}

type memoryVerifiedReader struct {
	*bytes.Reader
	ref domain.BlobRef
}

func (r *memoryVerifiedReader) Close() error            { return nil }
func (r *memoryVerifiedReader) BlobRef() domain.BlobRef { return r.ref }

type memoryArtifactWriter struct {
	mu          sync.Mutex
	store       *MemoryArtifactStore
	declaration port.ArtifactDeclaration
	callID      domain.AttemptCallID
	buffer      bytes.Buffer
	terminal    bool
}

func (w *memoryArtifactWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminal {
		return 0, fmt.Errorf("probe artifact writer is terminal")
	}
	if int64(w.buffer.Len()) > w.declaration.MaxBytes-int64(len(data)) {
		return 0, fmt.Errorf("probe artifact %q exceeds %d bytes", w.declaration.LogicalPath, w.declaration.MaxBytes)
	}
	return w.buffer.Write(data)
}

func (w *memoryArtifactWriter) Finalize(ctx context.Context) (domain.PendingArtifact, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.PendingArtifact{}, err
	}
	if w.terminal {
		return domain.PendingArtifact{}, fmt.Errorf("probe artifact writer is already terminal")
	}
	w.terminal = true
	ref, physicalNew := w.store.put(w.buffer.Bytes())
	physicalBytes := int64(0)
	if physicalNew {
		physicalBytes = ref.Size
	}
	return newPendingArtifact(ref, w.declaration, w.callID, physicalBytes)
}

func (w *memoryArtifactWriter) Abort(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.terminal = true
	return nil
}

func newPendingArtifact(blob domain.BlobRef, declaration port.ArtifactDeclaration, callID domain.AttemptCallID, physicalNewBytes int64) (domain.PendingArtifact, error) {
	reservation, err := domain.NewID("res")
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	writer, err := domain.NewID("writer")
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	pin, err := domain.NewID("pin")
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	artifact := domain.PendingArtifact{
		Blob: blob, MediaType: declaration.MediaType, Role: declaration.Role, LogicalPath: declaration.LogicalPath,
		CallID: callID, ReservationID: domain.ReservationID(reservation), WriterTokenID: domain.ArtifactWriterTokenID(writer),
		PinID: domain.BlobPinID(pin), PhysicalNewBytes: physicalNewBytes, Provenance: declaration.Provenance,
	}
	if err := artifact.Validate(); err != nil {
		return domain.PendingArtifact{}, err
	}
	return artifact, nil
}

var _ port.VerifiedBlobReader = (*MemoryArtifactStore)(nil)
var _ port.MeteredArtifactSink = (*MemoryArtifactStore)(nil)
var _ io.ReadCloser = (*memoryVerifiedReader)(nil)
