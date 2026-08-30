package domain

import "fmt"

type CompileOutcome string

const (
	CompileOK         CompileOutcome = "OK"
	CompileCE         CompileOutcome = "CE"
	CompileInfraError CompileOutcome = "INFRA_ERROR"
)

func (v CompileOutcome) Valid() bool {
	return v == CompileOK || v == CompileCE || v == CompileInfraError
}
func (v *CompileOutcome) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "CompileOutcome", func(raw string) bool { return CompileOutcome(raw).Valid() }, (*string)(v))
}

type ProcessOutcome string

const (
	ProcessExited     ProcessOutcome = "EXITED"
	ProcessSignaled   ProcessOutcome = "SIGNALED"
	ProcessTLE        ProcessOutcome = "TLE"
	ProcessMLE        ProcessOutcome = "MLE"
	ProcessOLE        ProcessOutcome = "OLE"
	ProcessInfraError ProcessOutcome = "INFRA_ERROR"
)

func (v ProcessOutcome) Valid() bool {
	switch v {
	case ProcessExited, ProcessSignaled, ProcessTLE, ProcessMLE, ProcessOLE, ProcessInfraError:
		return true
	default:
		return false
	}
}
func (v *ProcessOutcome) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "ProcessOutcome", func(raw string) bool { return ProcessOutcome(raw).Valid() }, (*string)(v))
}

type ValidatorOutcome string

const (
	ValidatorValid   ValidatorOutcome = "VALID"
	ValidatorInvalid ValidatorOutcome = "INVALID"
	ValidatorError   ValidatorOutcome = "VALIDATOR_ERROR"
)

func (v ValidatorOutcome) Valid() bool {
	return v == ValidatorValid || v == ValidatorInvalid || v == ValidatorError
}
func (v *ValidatorOutcome) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "ValidatorOutcome", func(raw string) bool { return ValidatorOutcome(raw).Valid() }, (*string)(v))
}

type CheckerOutcome string

const (
	CheckerAC    CheckerOutcome = "AC"
	CheckerWA    CheckerOutcome = "WA"
	CheckerPE    CheckerOutcome = "PE"
	CheckerError CheckerOutcome = "CHECKER_ERROR"
)

func (v CheckerOutcome) Valid() bool {
	return v == CheckerAC || v == CheckerWA || v == CheckerPE || v == CheckerError
}
func (v *CheckerOutcome) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "CheckerOutcome", func(raw string) bool { return CheckerOutcome(raw).Valid() }, (*string)(v))
}

type SolutionVerdict string

const (
	SolutionOK  SolutionVerdict = "OK"
	SolutionRE  SolutionVerdict = "RE"
	SolutionTLE SolutionVerdict = "TLE"
	SolutionMLE SolutionVerdict = "MLE"
	SolutionOLE SolutionVerdict = "OLE"
)

func (v SolutionVerdict) Valid() bool {
	return v == SolutionOK || v == SolutionRE || v == SolutionTLE || v == SolutionMLE || v == SolutionOLE
}
func (v *SolutionVerdict) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "SolutionVerdict", func(raw string) bool { return SolutionVerdict(raw).Valid() }, (*string)(v))
}

type FailureRouteClass string

const (
	RouteSandboxInfrastructure     FailureRouteClass = "SANDBOX_INFRA"
	RouteTrustedToolInfrastructure FailureRouteClass = "TRUSTED_TOOL_INFRASTRUCTURE"
	RouteVerificationContent       FailureRouteClass = "VERIFICATION_CONTENT"
	RouteGeneratedRepair           FailureRouteClass = "GENERATED_REPAIR"
)

func (v FailureRouteClass) Valid() bool {
	switch v {
	case RouteSandboxInfrastructure, RouteTrustedToolInfrastructure, RouteVerificationContent, RouteGeneratedRepair:
		return true
	default:
		return false
	}
}
func (v *FailureRouteClass) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "FailureRouteClass", func(raw string) bool { return FailureRouteClass(raw).Valid() }, (*string)(v))
}

type ExecutionCause string

const (
	CauseUserCancel          ExecutionCause = "user_cancel"
	CauseQuiesce             ExecutionCause = "quiesce"
	CauseRevisionInvalidated ExecutionCause = "revision_invalidated"
	CauseLeaseLost           ExecutionCause = "lease_lost"
	CauseStepDeadline        ExecutionCause = "step_deadline"
	CauseRunBudgetDeadline   ExecutionCause = "run_budget_deadline"
)

func (v ExecutionCause) Valid() bool {
	switch v {
	case CauseUserCancel, CauseQuiesce, CauseRevisionInvalidated, CauseLeaseLost, CauseStepDeadline, CauseRunBudgetDeadline:
		return true
	default:
		return false
	}
}
func (v *ExecutionCause) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "ExecutionCause", func(raw string) bool { return ExecutionCause(raw).Valid() }, (*string)(v))
}

type ExecutionInterrupted struct{ Cause ExecutionCause }

func (e ExecutionInterrupted) Error() string {
	return fmt.Sprintf("execution interrupted: %s", e.Cause)
}

func (e ExecutionInterrupted) Validate() error {
	if !e.Cause.Valid() {
		return fmt.Errorf("invalid execution interruption cause %q", e.Cause)
	}
	return nil
}
