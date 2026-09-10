package domain

import "errors"

// MutationBudgetSnapshot describes the shared stage quota for CONTENT and
// METADATA claims. It is a read projection, not permission to mutate content.
// AccountVersion is zero only when the stage account has not been created.
type MutationBudgetSnapshot struct {
	RunID          RunID     `json:"run_id"`
	StageName      StageName `json:"stage_name"`
	RunVersion     int64     `json:"run_version"`
	Limit          int64     `json:"limit"`
	Claimed        int64     `json:"claimed"`
	AccountVersion int64     `json:"account_version"`
}

func (s MutationBudgetSnapshot) Validate() error {
	if err := s.RunID.Validate(); err != nil {
		return err
	}
	if err := s.StageName.Validate(); err != nil {
		return err
	}
	if s.RunVersion <= 0 || s.Limit < 0 || s.Claimed < 0 || s.Claimed > s.Limit || s.AccountVersion < 0 || (s.AccountVersion == 0 && s.Claimed != 0) {
		return errors.New("invalid mutation budget projection")
	}
	return nil
}

func (s MutationBudgetSnapshot) Remaining() int64 {
	if s.Validate() != nil {
		return 0
	}
	return s.Limit - s.Claimed
}
