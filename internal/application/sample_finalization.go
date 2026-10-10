package application

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"cpgen/internal/artifact"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const sampleOutputMaxBytes = 1 << 16

// SamplePublicationFailure describes a deterministic restriction on the
// reference output displayed in an executed sample. An empty value passes.
type SamplePublicationFailure string

const (
	SamplePublicationInvalidText SamplePublicationFailure = "INVALID_TEXT"
	SamplePublicationTooLarge    SamplePublicationFailure = "TOO_LARGE"
)

func samplePublicationFailure(raw []byte) SamplePublicationFailure {
	if len(raw) > sampleOutputMaxBytes {
		return SamplePublicationTooLarge
	}
	if !utf8.Valid(raw) || strings.ContainsAny(string(raw), "\x00\r") {
		return SamplePublicationInvalidText
	}
	return ""
}

// FinalizeSamples is deterministic and read-only. Callers must first prove the
// report's committed provenance; structural validation is not that authority.
func FinalizeSamples(ctx context.Context, blobs port.VerifiedBlobReader, input JudgeInput, report JudgeVerificationReport) (domain.FinalizedStatement, error) {
	var empty domain.FinalizedStatement
	if blobs == nil {
		return empty, errors.New("sample finalization requires verified blobs")
	}
	if err := report.ValidateFor(input); err != nil {
		return empty, err
	}
	if !report.Passed {
		return empty, errors.New("sample finalization requires passing Judge")
	}
	digest, err := stableValueDigest(report)
	if err != nil {
		return empty, err
	}
	draft := input.DataInput.SolutionInput.Problem
	final := domain.FinalizedStatement{SchemaVersion: domain.FinalizedStatementSchema, DraftSpecDigest: draft.SpecDigest, JudgeReportDigest: digest, Language: draft.Language, Samples: make([]domain.ProblemSample, len(draft.Samples))}
	for i, sample := range draft.Samples {
		item := report.Cases[i]
		if item.Input.Origin != "sample" || item.Input.Ordinal != i+1 || item.Brute == nil || item.Answer == nil {
			return empty, errors.New("final sample lacks independent execution evidence")
		}
		raw, err := artifact.ReadVerified(ctx, blobs, *item.Answer, sampleOutputMaxBytes)
		if err != nil {
			return empty, err
		}
		if samplePublicationFailure(raw) != "" {
			return empty, errors.New("final sample output must be UTF-8 LF text without NUL")
		}
		// No unverified model explanation survives finalization. This bounded
		// explanation describes the displayed execution result, not a claimed
		// algorithm trace. Rich semantic explanations require separate review.
		explanation := domain.FinalSampleExplanation(draft.Language, string(raw))
		final.Samples[i] = domain.ProblemSample{Input: sample.Input, Output: string(raw), Explanation: explanation}
	}
	// The local presentation copy has no authority as a new ProblemSpec.
	draft.Samples = final.Samples
	final.StatementMarkdown = renderPackageStatement(draft)
	return final, final.Validate()
}
