package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/packageprobe"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

const generationPackageProducer = "mvp-package"

type PackageStageResult struct {
	Binding     domain.VerifiedPackageBinding
	Occurrences []domain.PendingOccurrence
}

func (s *PackageExecutor) Run(ctx context.Context, view domain.RunView) (PackageStageResult, error) {
	var empty PackageStageResult
	problem, quality, err := s.reader.Assemble(ctx, view)
	if err != nil {
		return empty, err
	}
	digest, err := stableValueDigest(quality)
	if err != nil {
		return empty, err
	}
	attempt, err := s.admission.admit(ctx, view, "package", digest)
	if err != nil {
		return empty, err
	}
	raw, manifest, err := packageprobe.BuildArchive(ctx, problem)
	if err != nil {
		return empty, err
	}
	if int64(len(raw)) > view.Budget().Limits.MaxPackageBytes {
		return empty, errors.New("package exceeds the run package byte limit")
	}
	verified, err := packageprobe.ReadArchive(ctx, raw)
	if err != nil || verified.Manifest.PackageID != manifest.PackageID {
		return empty, errors.Join(err, errors.New("package archive failed round-trip verification"))
	}
	manifestRaw, err := packageprobe.EncodeManifest(manifest)
	if err != nil {
		return empty, err
	}
	publisher, err := s.publisher(attempt, view.Version())
	if err != nil {
		return empty, err
	}
	packageSchema := packageprobe.GenerationPackageSchemaVersionV2
	if view.WorkflowRevision() == workflow.ExecutedSamplesRevision {
		packageSchema = packageprobe.GenerationPackageSchemaVersion
	}
	pending, err := publisher.Publish(ctx, port.ArtifactDeclaration{LogicalPath: "package/problem.zip", Role: domain.ArtifactOutput, MediaType: "application/zip", MaxBytes: int64(len(raw)), Provenance: domain.ProvenanceCandidate{SchemaVersion: packageSchema, Producer: generationPackageProducer, InputDigest: &digest}}, raw)
	if err != nil {
		return empty, err
	}
	return PackageStageResult{Binding: domain.VerifiedPackageBinding{PackageID: manifest.PackageID, ManifestDigest: domain.SumBytes(manifestRaw), QualityDigest: digest, Archive: pending.Blob}, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending}}}, nil
}

type PackageExecutor struct {
	reader    *PackageReader
	admission *StageAdmission
	publisher StagePublisher
}

func readPackageBlob(ctx context.Context, blobs port.VerifiedBlobReader, ref domain.BlobRef) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if ref.Size > 1<<20 {
		return nil, errors.New("package test exceeds byte bound")
	}
	r, err := blobs.OpenVerified(ctx, ref)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(r, ref.Size+1))
	if err := errors.Join(err, r.Close()); err != nil {
		return nil, err
	}
	if int64(len(raw)) != ref.Size || domain.SumBytes(raw) != ref.Digest {
		return nil, errors.New("package blob differs")
	}
	return raw, nil
}

func renderPackageStatement(spec domain.ProblemSpec) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n%s\n\n## Input\n\n%s\n", spec.Title, spec.Description, spec.Input.Description)
	for _, field := range spec.Input.Fields {
		fmt.Fprintf(&out, "\n- %s\n", field)
	}
	fmt.Fprintf(&out, "\n## Output\n\n%s\n", spec.Output.Description)
	for _, field := range spec.Output.Fields {
		fmt.Fprintf(&out, "\n- %s\n", field)
	}
	fmt.Fprintf(&out, "\n## Limits\n\n%d ms; %d MB\n", spec.TimeLimitMS, spec.MemoryLimitMB)
	for i, sample := range spec.Samples {
		fmt.Fprintf(&out, "\n## Sample %d\n\nInput:\n\n%s\n\nOutput:\n\n%s\n", i+1, fencedSample(sample.Input), fencedSample(sample.Output))
		if sample.Explanation != "" {
			fmt.Fprintf(&out, "\n%s\n", sample.Explanation)
		}
	}
	return out.String()
}

func fencedSample(raw string) string {
	fence := "```"
	for strings.Contains(raw, fence) {
		fence += "`"
	}
	return fence + "text\n" + raw + "\n" + fence
}

func (s *PackageExecutor) Reader() *PackageReader { return s.reader }
