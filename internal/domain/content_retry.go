package domain

import "errors"

// FinishContentRetryCommand atomically preserves a failed attempt and either
// schedules a bounded regeneration or finishes at the ordinary review gate.
// It never represents a human review decision or increases a budget.
type FinishContentRetryCommand struct {
	Finish FinishStageCommand `json:"finish"`
	Reason string             `json:"reason"`
}

func (v FinishContentRetryCommand) Validate() error {
	if err := v.Finish.Validate(); err != nil {
		return err
	}
	if v.Finish.AttemptState != StageAttemptNeedsReview || v.Finish.ReviewGateWaivable || v.Reason == "" || len(v.Reason) > 512 {
		return errors.New("content retry requires a bounded non-waivable review failure")
	}
	return nil
}
