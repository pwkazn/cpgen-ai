package judge

import (
	"fmt"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const TestlibAdapterV1 = "testlib_v1"

type TestlibExitCodes struct {
	OK   int
	WA   int
	PE   int
	Fail int
	Dirt int
}

var DefaultTestlibV1 = TestlibExitCodes{OK: 0, WA: 1, PE: 2, Fail: 3, Dirt: 4}

func (c TestlibExitCodes) Validate() error {
	values := []int{c.OK, c.WA, c.PE, c.Fail, c.Dirt}
	seen := make(map[int]struct{}, len(values))
	for _, value := range values {
		if value < 0 {
			return fmt.Errorf("testlib exit codes must be non-negative")
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("testlib exit codes must be distinct")
		}
		seen[value] = struct{}{}
	}
	return nil
}

type Adapted[T any] struct {
	Outcome               *T
	InfrastructureFailure bool
	Reason                string
}

func AdaptSolution(result port.RunResult) (Adapted[domain.SolutionVerdict], error) {
	if err := result.Validate(); err != nil {
		return Adapted[domain.SolutionVerdict]{}, err
	}
	if result.Outcome == domain.ProcessInfraError {
		return Adapted[domain.SolutionVerdict]{InfrastructureFailure: true}, nil
	}
	var verdict domain.SolutionVerdict
	switch result.Outcome {
	case domain.ProcessExited:
		if *result.ExitCode == 0 {
			verdict = domain.SolutionOK
		} else {
			verdict = domain.SolutionRE
		}
	case domain.ProcessSignaled:
		verdict = domain.SolutionRE
	case domain.ProcessTLE:
		verdict = domain.SolutionTLE
	case domain.ProcessMLE:
		verdict = domain.SolutionMLE
	case domain.ProcessOLE:
		verdict = domain.SolutionOLE
	default:
		return Adapted[domain.SolutionVerdict]{}, fmt.Errorf("unsupported process outcome %q", result.Outcome)
	}
	return Adapted[domain.SolutionVerdict]{Outcome: &verdict}, nil
}

func AdaptValidator(result port.RunResult, codes TestlibExitCodes) (Adapted[domain.ValidatorOutcome], error) {
	if err := codes.Validate(); err != nil {
		return Adapted[domain.ValidatorOutcome]{}, err
	}
	if err := result.Validate(); err != nil {
		return Adapted[domain.ValidatorOutcome]{}, err
	}
	if result.Outcome == domain.ProcessInfraError {
		return Adapted[domain.ValidatorOutcome]{InfrastructureFailure: true}, nil
	}
	outcome := domain.ValidatorError
	if result.Outcome == domain.ProcessExited {
		switch *result.ExitCode {
		case codes.OK:
			outcome = domain.ValidatorValid
		case codes.Fail:
			outcome = domain.ValidatorInvalid
		}
	}
	return Adapted[domain.ValidatorOutcome]{Outcome: &outcome}, nil
}

func AdaptChecker(result port.RunResult, codes TestlibExitCodes) (Adapted[domain.CheckerOutcome], error) {
	if err := codes.Validate(); err != nil {
		return Adapted[domain.CheckerOutcome]{}, err
	}
	if err := result.Validate(); err != nil {
		return Adapted[domain.CheckerOutcome]{}, err
	}
	if result.Outcome == domain.ProcessInfraError {
		return Adapted[domain.CheckerOutcome]{InfrastructureFailure: true}, nil
	}
	outcome := domain.CheckerError
	reason := ""
	if result.Outcome == domain.ProcessExited {
		switch *result.ExitCode {
		case codes.OK:
			outcome = domain.CheckerAC
		case codes.WA:
			outcome = domain.CheckerWA
		case codes.PE:
			outcome = domain.CheckerPE
		case codes.Dirt:
			outcome = domain.CheckerPE
			reason = "DIRT"
		}
	}
	return Adapted[domain.CheckerOutcome]{Outcome: &outcome, Reason: reason}, nil
}
