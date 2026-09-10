package application

import (
	"bytes"
	"context"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/packageprobe"
	"cpgen/internal/port"
)

// ReadArchive is a read-only export boundary. A package ledger entry does not
// bypass current source proof: reconstruct the complete expected archive and
// require the committed bytes, manifest and final run binding to match it.
func (s *PackageExecutor) ReadArchive(ctx context.Context, runID domain.RunID) ([]byte, domain.VerifiedPackageRecord, error) {
	var empty domain.VerifiedPackageRecord
	store, ok := s.quality.data.generation.config.Store.(interface {
		GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
		BudgetSnapshot(context.Context, domain.RunID) (domain.BudgetSnapshot, error)
		ReadVerifiedPackage(context.Context, domain.RunID) (domain.VerifiedPackageRecord, error)
		ReadCommittedSandboxStage(context.Context, domain.RunID, domain.StageName) (port.CommittedPrivateStage, error)
	})
	if !ok {
		return nil, empty, errors.New("package requires committed package and run readers")
	}
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return nil, empty, err
	}
	if run.State != domain.RunReady || run.FinalPackageOccurrenceID == nil {
		return nil, empty, errors.New("package export requires READY")
	}
	record, err := store.ReadVerifiedPackage(ctx, runID)
	if err != nil {
		return nil, empty, err
	}
	if record.Validate() != nil || record.RunID != runID || record.OccurrenceID != *run.FinalPackageOccurrenceID {
		return nil, empty, errors.New("package record differs from final run binding")
	}
	budget, err := store.BudgetSnapshot(ctx, runID)
	if err != nil {
		return nil, empty, err
	}
	view, err := domain.NewRunViewFromSnapshot(run, budget, nil)
	if err != nil {
		return nil, empty, err
	}
	problem, quality, err := s.Assemble(ctx, view)
	if err != nil {
		return nil, empty, err
	}
	qualityDigest, err := stableValueDigest(quality)
	if err != nil || qualityDigest != record.Binding.QualityDigest {
		return nil, empty, errors.New("package lost its current Quality report")
	}
	expected, manifest, err := packageprobe.BuildArchive(ctx, problem)
	if err != nil {
		return nil, empty, err
	}
	manifestRaw, err := packageprobe.EncodeManifest(manifest)
	if err != nil {
		return nil, empty, err
	}
	if manifest.PackageID != record.Binding.PackageID || domain.SumBytes(manifestRaw) != record.Binding.ManifestDigest || domain.SumBytes(expected) != record.Binding.Archive.Digest || int64(len(expected)) != record.Binding.Archive.Size {
		return nil, empty, errors.New("package differs from reconstructed current artifacts")
	}
	stage, err := store.ReadCommittedSandboxStage(ctx, runID, "package")
	if err != nil {
		return nil, empty, err
	}
	if stage.Attempt.AttemptID != record.AttemptID || stage.Attempt.InputDigest != qualityDigest || stage.Attempt.OutputDigest == nil || *stage.Attempt.OutputDigest != record.Binding.Archive.Digest || len(stage.Artifacts) != 1 {
		return nil, empty, errors.New("package stage differs from verified record")
	}
	item := stage.Artifacts[0]
	if item.OccurrenceID != record.OccurrenceID || item.Blob.Blob != record.Binding.Archive || item.Blob.LogicalPath != "package/problem.zip" || item.Blob.Role != domain.ArtifactOutput || item.Blob.MediaType != "application/zip" || item.Blob.Provenance.SchemaVersion != "cpgen.package/v2" || item.Blob.Provenance.Producer != generationPackageProducer || item.Blob.Provenance.InputDigest == nil || *item.Blob.Provenance.InputDigest != qualityDigest {
		return nil, empty, errors.New("package archive provenance differs")
	}
	raw, err := readSolutionVerificationBlob(ctx, s.quality.data.generation.config.Blobs, record.Binding.Archive, 65<<20)
	if err != nil {
		return nil, empty, err
	}
	if !bytes.Equal(raw, expected) {
		return nil, empty, errors.New("committed package bytes differ")
	}
	if _, err := packageprobe.ReadArchive(ctx, raw); err != nil {
		return nil, empty, err
	}
	return raw, record, nil
}
