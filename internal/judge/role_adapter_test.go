package judge_test

import (
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/port"
)

const testCall domain.AttemptCallID = "call_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestCompileLayerVectors(t *testing.T) {
	t.Parallel()
	trace := domain.CallTrace{LogicalOperationID: "compile-vector", DispatchKind: domain.DispatchDispatched,
		PhysicalAttemptCallIDs: []domain.AttemptCallID{testCall}, ResultAttemptCallID: callPointer(testCall)}
	for _, outcome := range []domain.CompileOutcome{domain.CompileCE, domain.CompileInfraError} {
		result := port.CompileResult{CallTrace: trace, Outcome: outcome}
		if err := result.Validate(); err != nil {
			t.Errorf("compile outcome %s was rejected: %v", outcome, err)
		}
	}
}

func TestSolutionVectors(t *testing.T) {
	t.Parallel()
	zero, fortyTwo := 0, 42
	sigsegv := "SIGSEGV"
	tests := []struct {
		name    string
		outcome domain.ProcessOutcome
		exit    *int
		signal  *string
		want    domain.SolutionVerdict
		infra   bool
	}{
		{"exit 0", domain.ProcessExited, &zero, nil, domain.SolutionOK, false},
		{"exit 42", domain.ProcessExited, &fortyTwo, nil, domain.SolutionRE, false},
		{"signal", domain.ProcessSignaled, nil, &sigsegv, domain.SolutionRE, false},
		{"TLE", domain.ProcessTLE, nil, nil, domain.SolutionTLE, false},
		{"MLE", domain.ProcessMLE, nil, nil, domain.SolutionMLE, false},
		{"OLE", domain.ProcessOLE, nil, nil, domain.SolutionOLE, false},
		{"infrastructure", domain.ProcessInfraError, nil, nil, "", true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapted, err := judge.AdaptSolution(runResult(test.outcome, test.exit, test.signal))
			if err != nil {
				t.Fatal(err)
			}
			if adapted.InfrastructureFailure != test.infra {
				t.Fatalf("infrastructure = %v, want %v", adapted.InfrastructureFailure, test.infra)
			}
			if test.infra {
				if adapted.Outcome != nil {
					t.Fatal("infrastructure result produced a solution verdict")
				}
				return
			}
			if adapted.Outcome == nil || *adapted.Outcome != test.want {
				t.Fatalf("outcome = %v, want %s", adapted.Outcome, test.want)
			}
		})
	}
}

func TestValidatorVectors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		outcome domain.ProcessOutcome
		exit    *int
		signal  *string
		want    domain.ValidatorOutcome
		infra   bool
	}{
		{"valid", domain.ProcessExited, intPointer(0), nil, domain.ValidatorValid, false},
		{"invalid", domain.ProcessExited, intPointer(3), nil, domain.ValidatorInvalid, false},
		{"unknown", domain.ProcessExited, intPointer(9), nil, domain.ValidatorError, false},
		{"signal", domain.ProcessSignaled, nil, stringPointer("SIGSEGV"), domain.ValidatorError, false},
		{"TLE", domain.ProcessTLE, nil, nil, domain.ValidatorError, false},
		{"MLE", domain.ProcessMLE, nil, nil, domain.ValidatorError, false},
		{"OLE", domain.ProcessOLE, nil, nil, domain.ValidatorError, false},
		{"infrastructure", domain.ProcessInfraError, nil, nil, "", true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapted, err := judge.AdaptValidator(runResult(test.outcome, test.exit, test.signal), judge.DefaultTestlibV1)
			if err != nil {
				t.Fatal(err)
			}
			assertAdapted(t, adapted.Outcome, adapted.InfrastructureFailure, test.want, test.infra)
		})
	}
}

func TestCheckerVectors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		outcome domain.ProcessOutcome
		exit    *int
		signal  *string
		want    domain.CheckerOutcome
		reason  string
		infra   bool
	}{
		{"AC", domain.ProcessExited, intPointer(0), nil, domain.CheckerAC, "", false},
		{"WA", domain.ProcessExited, intPointer(1), nil, domain.CheckerWA, "", false},
		{"PE", domain.ProcessExited, intPointer(2), nil, domain.CheckerPE, "", false},
		{"DIRT", domain.ProcessExited, intPointer(4), nil, domain.CheckerPE, "DIRT", false},
		{"fail", domain.ProcessExited, intPointer(3), nil, domain.CheckerError, "", false},
		{"unknown", domain.ProcessExited, intPointer(9), nil, domain.CheckerError, "", false},
		{"signal", domain.ProcessSignaled, nil, stringPointer("SIGSEGV"), domain.CheckerError, "", false},
		{"TLE", domain.ProcessTLE, nil, nil, domain.CheckerError, "", false},
		{"MLE", domain.ProcessMLE, nil, nil, domain.CheckerError, "", false},
		{"OLE", domain.ProcessOLE, nil, nil, domain.CheckerError, "", false},
		{"infrastructure", domain.ProcessInfraError, nil, nil, "", "", true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapted, err := judge.AdaptChecker(runResult(test.outcome, test.exit, test.signal), judge.DefaultTestlibV1)
			if err != nil {
				t.Fatal(err)
			}
			assertAdapted(t, adapted.Outcome, adapted.InfrastructureFailure, test.want, test.infra)
			if adapted.Reason != test.reason {
				t.Fatalf("reason = %q, want %q", adapted.Reason, test.reason)
			}
		})
	}
}

func TestTestlibExitCodesMustBeDistinct(t *testing.T) {
	t.Parallel()
	codes := judge.DefaultTestlibV1
	codes.Fail = codes.WA
	if _, err := judge.AdaptChecker(runResult(domain.ProcessExited, intPointer(0), nil), codes); err == nil {
		t.Fatal("overlapping testlib exit codes were accepted")
	}
}

func runResult(outcome domain.ProcessOutcome, exit *int, signal *string) port.RunResult {
	return port.RunResult{
		CallTrace: domain.CallTrace{LogicalOperationID: "judge-vector", DispatchKind: domain.DispatchDispatched,
			PhysicalAttemptCallIDs: []domain.AttemptCallID{testCall}, ResultAttemptCallID: callPointer(testCall)},
		Outcome: outcome, ExitCode: exit, Signal: signal,
	}
}

func assertAdapted[T comparable](t *testing.T, got *T, gotInfra bool, want T, wantInfra bool) {
	t.Helper()
	if gotInfra != wantInfra {
		t.Fatalf("infrastructure = %v, want %v", gotInfra, wantInfra)
	}
	if wantInfra {
		if got != nil {
			t.Fatal("infrastructure result produced a business outcome")
		}
		return
	}
	if got == nil || *got != want {
		t.Fatalf("outcome = %v, want %v", got, want)
	}
}

func intPointer(value int) *int                                    { return &value }
func stringPointer(value string) *string                           { return &value }
func callPointer(value domain.AttemptCallID) *domain.AttemptCallID { return &value }
