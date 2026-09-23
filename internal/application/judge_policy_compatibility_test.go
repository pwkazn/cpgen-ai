package application

import (
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestJudgePolicyDigestPreservesHistoricalReports(t *testing.T) {
	// Freeze the pre-V3 policy's exact JSON inputs: 2 seconds, 512 MiB,
	// 64 processes, 1 MiB per stream, and this fixed toolchain identity.
	// The expected digests were calculated from the historical JSON encoding,
	// independently of judgePolicyDigest, and must not be regenerated with it.
	input := JudgeInput{
		DataInput: domain.DataDraftInputV1{SolutionInput: domain.SolutionDraftInputV1{
			Problem: domain.ProblemSpec{TimeLimitMS: 2000, MemoryLimitMB: 512},
		}},
		DataReport: DataVerificationReport{
			ToolchainLockDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		},
	}
	const historical = domain.Digest("sha256:67907617ac1924edbb09c75f9f954990cec00b31dc10454868c116dc41a2be98")
	const executed = domain.Digest("sha256:767032ba68e802f2fc84939420cf2167487aa165f25a0630464edbe75af6cfa2")
	for _, revision := range []string{workflow.GenerationRevision, workflow.RetryingGenerationRevision} {
		t.Run(revision, func(t *testing.T) {
			input.WorkflowRevision = revision
			if got := judgePolicyDigest(input); got != historical {
				t.Fatalf("default historical policy = %s, want %s", got, historical)
			}
			if got := judgePolicyDigest(input, judgeVerificationSchema); got != historical {
				t.Fatalf("stored V1 report policy = %s, want %s", got, historical)
			}
		})
	}
	input.WorkflowRevision = workflow.ExecutedSamplesRevision
	if got := judgePolicyDigest(input, executedJudgeVerificationSchema); got != executed {
		t.Fatalf("executed sample policy = %s, want %s", got, executed)
	}
}
