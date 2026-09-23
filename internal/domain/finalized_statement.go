package domain

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// FinalizedStatement is a separate immutable publication, never a rewritten
// ProblemSpec. The Judge digest binds validation, small-case differential
// checks and independent executions on each exact sample input.
type FinalizedStatement struct {
	SchemaVersion     string          `json:"schema_version"`
	DraftSpecDigest   Digest          `json:"draft_spec_digest"`
	JudgeReportDigest Digest          `json:"judge_report_digest"`
	Language          string          `json:"language"`
	StatementMarkdown string          `json:"statement_markdown"`
	Samples           []ProblemSample `json:"samples"`
}

const FinalizedStatementSchema = "cpgen.finalized-statement/v1"

// FinalSampleExplanation makes only machine-checkable claims about the exact
// output. It deliberately makes no unverified algorithm-trace claims.
func FinalSampleExplanation(language, output string) string {
	count := len(strings.Fields(output))
	if strings.HasPrefix(language, "zh") {
		return fmt.Sprintf("对于此样例输入，输出按上方所示顺序包含 %d 个标记。", count)
	}
	return fmt.Sprintf("For this sample input, the output contains %d tokens in the order shown above.", count)
}

func (f FinalizedStatement) Validate() error {
	if f.SchemaVersion != FinalizedStatementSchema || f.DraftSpecDigest.Validate() != nil || f.JudgeReportDigest.Validate() != nil || validateText(32768, false, f.Language) != nil || len(f.Samples) < 1 || len(f.Samples) > 32 || !utf8.ValidString(f.StatementMarkdown) || len(f.StatementMarkdown) > 1<<20 {
		return errors.New("invalid finalized statement binding or bounds")
	}
	for _, sample := range f.Samples {
		if validateSampleData(sample.Input, sample.Output) != nil || sample.Explanation != FinalSampleExplanation(f.Language, sample.Output) || !strings.Contains(f.StatementMarkdown, sample.Explanation) {
			return errors.New("final sample explanation differs from its output or statement")
		}
	}
	return nil
}
