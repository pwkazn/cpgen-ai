//go:build cpgen_slice0_probe

package probe

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/fixture/ab"
	"cpgen/internal/judge"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

const VerticalReportSchemaVersion domain.SchemaVersion = "cpgen.slice0-vertical/v1"

type VerticalStatus string

const (
	VerticalPassed  VerticalStatus = "PASSED"
	VerticalFailed  VerticalStatus = "FAILED"
	VerticalBlocked VerticalStatus = "BLOCKED"
)

type VerticalCaseReport struct {
	Input     domain.BlobRef        `json:"input"`
	Reference domain.CheckerOutcome `json:"reference"`
	Brute     domain.CheckerOutcome `json:"brute"`
}

type VerticalReport struct {
	SchemaVersion        domain.SchemaVersion             `json:"schema_version"`
	Status               VerticalStatus                   `json:"status"`
	Programs             map[string]domain.BlobRef        `json:"programs"`
	GeneratedCasesDigest domain.Digest                    `json:"generated_cases_digest"`
	ValidatorLegal       domain.ValidatorOutcome          `json:"validator_legal"`
	ValidatorIllegal     domain.ValidatorOutcome          `json:"validator_illegal"`
	Cases                []VerticalCaseReport             `json:"cases"`
	CheckerAttacks       map[string]domain.CheckerOutcome `json:"checker_attacks"`
	CallTraces           []domain.CallTrace               `json:"call_traces"`
	Failure              *domain.PortFailure              `json:"failure,omitempty"`
	Reason               string                           `json:"reason,omitempty"`
}

func (r VerticalReport) Validate() error {
	if r.SchemaVersion != VerticalReportSchemaVersion {
		return fmt.Errorf("vertical report schema must be %q", VerticalReportSchemaVersion)
	}
	if r.Status != VerticalPassed && r.Status != VerticalFailed && r.Status != VerticalBlocked {
		return fmt.Errorf("invalid vertical status %q", r.Status)
	}
	for index, trace := range r.CallTraces {
		if err := trace.Validate(); err != nil {
			return fmt.Errorf("vertical CallTrace %d: %w", index, err)
		}
	}
	if r.Status == VerticalBlocked {
		if r.Failure == nil || r.Failure.Class != domain.FailureBlocked && r.Failure.Class != domain.FailureIncompatible {
			return fmt.Errorf("blocked vertical report requires a blocked or incompatible PortFailure")
		}
		return r.Failure.Validate()
	}
	if r.Failure != nil || r.Reason == "" && r.Status == VerticalFailed {
		return fmt.Errorf("content report failure fields are inconsistent")
	}
	if r.Status != VerticalPassed {
		return nil
	}
	if len(r.Programs) != 5 || len(r.Cases) != len(ab.GeneratedCases(ab.FixedSeed)) {
		return fmt.Errorf("passed vertical report is incomplete")
	}
	for name, blob := range r.Programs {
		if name == "" {
			return fmt.Errorf("vertical program name is empty")
		}
		if err := blob.Validate(); err != nil {
			return err
		}
	}
	if err := r.GeneratedCasesDigest.Validate(); err != nil {
		return err
	}
	if r.ValidatorLegal != domain.ValidatorValid || r.ValidatorIllegal != domain.ValidatorInvalid {
		return fmt.Errorf("validator boundary evidence is incomplete")
	}
	for _, test := range r.Cases {
		if err := test.Input.Validate(); err != nil {
			return err
		}
		if test.Reference != domain.CheckerAC || test.Brute != domain.CheckerAC {
			return fmt.Errorf("differential case did not pass through CheckerOutcome")
		}
	}
	wantAttacks := map[string]domain.CheckerOutcome{
		"wrong": domain.CheckerWA, "malformed": domain.CheckerPE,
		"trailing": domain.CheckerPE, "bad-answer": domain.CheckerError,
	}
	for name, want := range wantAttacks {
		if r.CheckerAttacks[name] != want {
			return fmt.Errorf("checker attack %q = %q, want %q", name, r.CheckerAttacks[name], want)
		}
	}
	return nil
}

func (h *Harness) RunVertical(ctx context.Context) (VerticalReport, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	report := VerticalReport{
		SchemaVersion: VerticalReportSchemaVersion, Status: VerticalFailed,
		Programs: map[string]domain.BlobRef{}, CheckerAttacks: map[string]domain.CheckerOutcome{},
	}
	programs := map[string]domain.PendingArtifact{}
	for _, definition := range ab.Definitions() {
		bundle, err := ab.Bundle(h.artifacts, definition.Name)
		if err != nil {
			return report, err
		}
		request := port.CompileRequest{
			Language: port.LanguageCPP20, Role: definition.Role, SourceBundle: bundle, Toolchain: toolchain.CPP20ToolchainID,
			Limits:         port.CompileLimits{Time: 45 * time.Second, MemoryBytes: 512 << 20, PIDs: 64, OutputBytes: 8 << 20},
			ExpectedOutput: "result/files/main",
		}
		result, runErr := h.compile(ctx, h.nextLogical("vertical-compile-"+definition.Name), request)
		report.addTrace(result.CallTrace)
		if runErr != nil {
			return blockedVertical(report, runErr), nil
		}
		if result.Outcome != domain.CompileOK || result.Program == nil {
			return failedVertical(report, fmt.Sprintf("fixture %s compile outcome is %q", definition.Name, result.Outcome)), nil
		}
		programs[definition.Name] = *result.Program
		report.Programs[definition.Name] = result.Program.Blob
	}

	legal := []byte("1 2\n")
	legalResult, err := h.runFixture(ctx, programs["validator"].Blob, port.RoleValidator, legal, nil, nil)
	report.addTrace(legalResult.CallTrace)
	if err != nil {
		return blockedVertical(report, err), nil
	}
	legalOutcome, infrastructure, err := validatorOutcome(legalResult)
	if err != nil {
		return report, err
	}
	if infrastructure {
		return blockedVertical(report, fmt.Errorf("validator infrastructure failure")), nil
	}
	report.ValidatorLegal = legalOutcome

	illegalResult, err := h.runFixture(ctx, programs["validator"].Blob, port.RoleValidator, []byte("1 2 trailing\n"), nil, nil)
	report.addTrace(illegalResult.CallTrace)
	if err != nil {
		return blockedVertical(report, err), nil
	}
	illegalOutcome, infrastructure, err := validatorOutcome(illegalResult)
	if err != nil {
		return report, err
	}
	if infrastructure {
		return blockedVertical(report, fmt.Errorf("validator infrastructure failure")), nil
	}
	report.ValidatorIllegal = illegalOutcome
	if legalOutcome != domain.ValidatorValid || illegalOutcome != domain.ValidatorInvalid {
		return failedVertical(report, "validator accepted/rejected the fixed boundary cases incorrectly"), nil
	}

	seed := ab.FixedSeed
	generatorResult, err := h.runFixture(ctx, programs["generator"].Blob, port.RoleGenerator, nil, &seed, nil)
	report.addTrace(generatorResult.CallTrace)
	if err != nil {
		return blockedVertical(report, err), nil
	}
	generatorVerdict, infrastructure, err := solutionOutcome(generatorResult)
	if err != nil {
		return report, err
	}
	if infrastructure {
		return blockedVertical(report, fmt.Errorf("generator infrastructure failure")), nil
	}
	if generatorVerdict != domain.SolutionOK || generatorResult.Stdout == nil {
		return failedVertical(report, "generator did not exit successfully with stdout"), nil
	}
	generated, err := h.artifacts.ReadBlob(generatorResult.Stdout.Blob)
	if err != nil {
		return report, err
	}
	wantGenerated := ab.RenderCases(ab.GeneratedCases(seed))
	if string(generated) != wantGenerated {
		return failedVertical(report, "generator bytes differ from the fixed seed contract"), nil
	}
	report.GeneratedCasesDigest = domain.SumBytes(generated)

	for _, test := range ab.GeneratedCases(seed) {
		input := []byte(strconv.FormatInt(test.A, 10) + " " + strconv.FormatInt(test.B, 10) + "\n")
		validation, runErr := h.runFixture(ctx, programs["validator"].Blob, port.RoleValidator, input, nil, nil)
		report.addTrace(validation.CallTrace)
		if runErr != nil {
			return blockedVertical(report, runErr), nil
		}
		valid, infrastructure, adaptErr := validatorOutcome(validation)
		if adaptErr != nil {
			return report, adaptErr
		}
		if infrastructure {
			return blockedVertical(report, fmt.Errorf("validator infrastructure failure")), nil
		}
		if valid != domain.ValidatorValid {
			return failedVertical(report, "generator emitted an invalid test"), nil
		}

		reference, runErr := h.runFixture(ctx, programs["reference"].Blob, port.RoleSolution, input, nil, nil)
		report.addTrace(reference.CallTrace)
		if runErr != nil {
			return blockedVertical(report, runErr), nil
		}
		brute, runErr := h.runFixture(ctx, programs["brute"].Blob, port.RoleBrute, input, nil, nil)
		report.addTrace(brute.CallTrace)
		if runErr != nil {
			return blockedVertical(report, runErr), nil
		}
		for name, result := range map[string]port.RunResult{"reference": reference, "brute": brute} {
			verdict, infrastructure, adaptErr := solutionOutcome(result)
			if adaptErr != nil {
				return report, adaptErr
			}
			if infrastructure {
				return blockedVertical(report, fmt.Errorf("%s infrastructure failure", name)), nil
			}
			if verdict != domain.SolutionOK || result.Stdout == nil {
				return failedVertical(report, name+" did not produce a successful answer"), nil
			}
		}
		referenceOutput, err := h.artifacts.ReadBlob(reference.Stdout.Blob)
		if err != nil {
			return report, err
		}
		bruteOutput, err := h.artifacts.ReadBlob(brute.Stdout.Blob)
		if err != nil {
			return report, err
		}
		referenceCheck, err := h.runChecker(ctx, programs["checker"].Blob, input, referenceOutput, referenceOutput)
		report.addTrace(referenceCheck.CallTrace)
		if err != nil {
			return blockedVertical(report, err), nil
		}
		bruteCheck, err := h.runChecker(ctx, programs["checker"].Blob, input, bruteOutput, referenceOutput)
		report.addTrace(bruteCheck.CallTrace)
		if err != nil {
			return blockedVertical(report, err), nil
		}
		referenceOutcome, blocked, err := checkerOutcome(referenceCheck)
		if err != nil {
			return report, err
		}
		if blocked {
			return blockedVertical(report, fmt.Errorf("checker infrastructure failure")), nil
		}
		bruteOutcome, blocked, err := checkerOutcome(bruteCheck)
		if err != nil {
			return report, err
		}
		if blocked {
			return blockedVertical(report, fmt.Errorf("checker infrastructure failure")), nil
		}
		report.Cases = append(report.Cases, VerticalCaseReport{Input: h.artifacts.PutBlob(input), Reference: referenceOutcome, Brute: bruteOutcome})
		if referenceOutcome != domain.CheckerAC || bruteOutcome != domain.CheckerAC {
			return failedVertical(report, "reference/brute differential checker outcome is not AC"), nil
		}
	}

	attackInput := []byte("1 2\n")
	for _, attack := range []struct {
		name   string
		output []byte
		answer []byte
	}{
		{name: "wrong", output: []byte("4\n"), answer: []byte("3\n")},
		{name: "malformed", output: []byte("not-an-integer\n"), answer: []byte("3\n")},
		{name: "trailing", output: []byte("3 trailing\n"), answer: []byte("3\n")},
		{name: "bad-answer", output: []byte("3\n"), answer: []byte("broken\n")},
	} {
		result, runErr := h.runChecker(ctx, programs["checker"].Blob, attackInput, attack.output, attack.answer)
		report.addTrace(result.CallTrace)
		if runErr != nil {
			return blockedVertical(report, runErr), nil
		}
		outcome, infrastructure, adaptErr := checkerOutcome(result)
		if adaptErr != nil {
			return report, adaptErr
		}
		if infrastructure {
			return blockedVertical(report, fmt.Errorf("checker attack infrastructure failure")), nil
		}
		report.CheckerAttacks[attack.name] = outcome
	}
	report.Status = VerticalPassed
	report.Reason = "fixed A+B compile, validation, generation, differential, and checker sequence passed"
	if err := report.Validate(); err != nil {
		return report, err
	}
	return report, nil
}

func (h *Harness) runFixture(ctx context.Context, program domain.BlobRef, role port.ProgramRole, stdin []byte, seed *uint64, files []port.InputMount) (port.RunResult, error) {
	request := port.RunRequest{
		Role: role, Program: program, Files: files, Seed: seed,
		Limits: port.RunLimits{Time: 5 * time.Second, MemoryBytes: 64 << 20, PIDs: 16, StdoutBytes: 64 << 10, StderrBytes: 64 << 10},
	}
	if stdin != nil {
		ref := h.artifacts.PutBlob(stdin)
		request.Stdin = &ref
	}
	return h.run(ctx, h.nextLogical("vertical-run-"+string(role)), request)
}

func (h *Harness) runChecker(ctx context.Context, program domain.BlobRef, input, output, answer []byte) (port.RunResult, error) {
	files := []port.InputMount{
		{Path: "input.txt", Blob: h.artifacts.PutBlob(input)},
		{Path: "output.txt", Blob: h.artifacts.PutBlob(output)},
		{Path: "answer.txt", Blob: h.artifacts.PutBlob(answer)},
	}
	return h.runFixture(ctx, program, port.RoleChecker, nil, nil, files)
}

func validatorOutcome(result port.RunResult) (domain.ValidatorOutcome, bool, error) {
	adapted, err := judge.AdaptValidator(result, judge.DefaultTestlibV1)
	if err != nil || adapted.Outcome == nil && !adapted.InfrastructureFailure {
		return "", false, errors.Join(err, fmt.Errorf("validator adapter omitted an outcome"))
	}
	if adapted.InfrastructureFailure {
		return "", true, nil
	}
	return *adapted.Outcome, false, nil
}

func checkerOutcome(result port.RunResult) (domain.CheckerOutcome, bool, error) {
	adapted, err := judge.AdaptChecker(result, judge.DefaultTestlibV1)
	if err != nil || adapted.Outcome == nil && !adapted.InfrastructureFailure {
		return "", false, errors.Join(err, fmt.Errorf("checker adapter omitted an outcome"))
	}
	if adapted.InfrastructureFailure {
		return "", true, nil
	}
	return *adapted.Outcome, false, nil
}

func solutionOutcome(result port.RunResult) (domain.SolutionVerdict, bool, error) {
	adapted, err := judge.AdaptSolution(result)
	if err != nil || adapted.Outcome == nil && !adapted.InfrastructureFailure {
		return "", false, errors.Join(err, fmt.Errorf("solution adapter omitted an outcome"))
	}
	if adapted.InfrastructureFailure {
		return "", true, nil
	}
	return *adapted.Outcome, false, nil
}

func (r *VerticalReport) addTrace(trace domain.CallTrace) {
	if trace.Validate() == nil {
		r.CallTraces = append(r.CallTraces, trace)
	}
}

func failedVertical(report VerticalReport, reason string) VerticalReport {
	report.Status = VerticalFailed
	report.Reason = reason
	return report
}

func blockedVertical(report VerticalReport, cause error) VerticalReport {
	report.Status = VerticalBlocked
	report.Reason = cause.Error()
	failure := domain.PortFailure{Code: domain.FailureUnavailable, Class: domain.FailureBlocked}
	var check *docker.CheckError
	if errors.As(cause, &check) {
		failure = check.Failure
		if failure.Class != domain.FailureBlocked && failure.Class != domain.FailureIncompatible {
			failure.Class = domain.FailureBlocked
		}
	}
	report.Failure = &failure
	return report
}
