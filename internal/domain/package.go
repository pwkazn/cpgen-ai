package domain

import "errors"

// VerifiedPackageBinding is issued only after the application verifies the
// archive against the current committed Quality and all of its upstream proofs.
// Its hashes alone are not proof; persistence also binds the owned publication.
type VerifiedPackageBinding struct {
	PackageID      Digest  `json:"package_id"`
	ManifestDigest Digest  `json:"manifest_digest"`
	QualityDigest  Digest  `json:"quality_digest"`
	Archive        BlobRef `json:"archive"`
}

type FinalizeVerifiedPackageCommand struct {
	Finish  FinishStageCommand     `json:"finish"`
	Package VerifiedPackageBinding `json:"package"`
}

type VerifiedPackageRecord struct {
	RunID        RunID                  `json:"run_id"`
	OccurrenceID ArtifactOccurrenceID   `json:"occurrence_id"`
	AttemptID    AttemptID              `json:"attempt_id"`
	Binding      VerifiedPackageBinding `json:"binding"`
}

func (v VerifiedPackageRecord) Validate() error {
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if err := v.OccurrenceID.Validate(); err != nil {
		return err
	}
	if err := v.AttemptID.Validate(); err != nil {
		return err
	}
	return v.Binding.Validate()
}

func (v VerifiedPackageBinding) Validate() error {
	for _, digest := range []Digest{v.PackageID, v.ManifestDigest, v.QualityDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	return v.Archive.Validate()
}

func (v FinalizeVerifiedPackageCommand) Validate() error {
	if err := v.Package.Validate(); err != nil {
		return err
	}
	f := v.Finish
	if err := f.Validate(); err != nil {
		return err
	}
	// The ordinary boundary remains RUNNING; only this dedicated operation
	// may replace that internal boundary with READY in the same transaction.
	if f.StageName != "package" || f.NextStage != "package" || f.RunState != RunRunning || f.AttemptState != StageAttemptSucceeded || f.OutputDigest == nil || *f.OutputDigest != v.Package.Archive.Digest || f.NextInputDigest == nil || *f.NextInputDigest != *f.OutputDigest || len(f.Occurrences) != 1 {
		return errors.New("invalid verified package completion boundary")
	}
	item := f.Occurrences[0]
	if item.Kind != PendingOccurrenceNewWrite || item.NewWrite == nil {
		return errors.New("package requires its owned archive publication")
	}
	p := item.NewWrite
	if p.Blob != v.Package.Archive || p.Role != ArtifactOutput || p.LogicalPath != "package/problem.zip" || p.MediaType != "application/zip" || (p.Provenance.SchemaVersion != "cpgen.package/v2" && p.Provenance.SchemaVersion != "cpgen.package/v3") || p.Provenance.Producer != "mvp-package" || p.Provenance.InputDigest == nil || *p.Provenance.InputDigest != v.Package.QualityDigest {
		return errors.New("package publication differs from verified binding")
	}
	return nil
}
