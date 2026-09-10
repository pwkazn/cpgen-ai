package domain

import "errors"

// FinishMutationStageCommand binds output attachment, complete attempt usage,
// the immutable mutation record and the next stage in one storage transaction.
// It consumes an existing claim; route authorization is a separate boundary.
type FinishMutationStageCommand struct {
	Finish   FinishStageCommand  `json:"finish"`
	Mutation MutationStageRecord `json:"mutation"`
}

// The occurrence ID is allocated during attachment. The command names its
// finalized writer instead, so its identity remains stable before commit.
type MutationStageRecord struct {
	Grant               MutationGrant         `json:"grant"`
	RecordID            string                `json:"record_id"`
	Operations          []MutationOperation   `json:"operations"`
	Reservations        []MutationReservation `json:"reservations"`
	OutputWriterTokenID ArtifactWriterTokenID `json:"output_writer_token_id"`
	OutputBlobDigest    Digest                `json:"output_blob_digest"`
}

func (v FinishMutationStageCommand) Validate() error {
	if err := v.Finish.Validate(); err != nil {
		return err
	}
	finish, record := v.Finish, v.Mutation
	if finish.AttemptState != StageAttemptSucceeded || record.Grant.RunID != finish.RunID || record.Grant.StageName != finish.StageName {
		return errors.New("mutation finalization requires a successful stage in the grant scope")
	}
	if err := (MutationRecordRequest{Grant: record.Grant, RecordID: record.RecordID, Operations: record.Operations, Reservations: record.Reservations}).validateEvidence(); err != nil {
		return err
	}
	if err := record.OutputWriterTokenID.Validate(); err != nil {
		return err
	}
	if err := record.OutputBlobDigest.Validate(); err != nil {
		return err
	}
	for _, operation := range record.Operations {
		if operation.AttemptID != finish.AttemptID {
			return errors.New("mutation finalization operations must belong to the finishing attempt")
		}
	}
	found := false
	for _, occurrence := range finish.Occurrences {
		if occurrence.Kind != PendingOccurrenceNewWrite {
			return errors.New("mutation finalization has no cache reuse policy")
		}
		output := occurrence.NewWrite
		if output.WriterTokenID != record.OutputWriterTokenID {
			continue
		}
		if found || output.Role != ArtifactOutput || output.Blob.Digest != record.OutputBlobDigest {
			return errors.New("mutation output writer must uniquely bind the committed output digest")
		}
		found = true
		reservationFound := false
		for _, reservation := range record.Reservations {
			if reservation.ReservationID == output.ReservationID && reservation.AttemptCallID == output.CallID {
				reservationFound = true
			}
		}
		if !reservationFound {
			return errors.New("mutation output requires its actual physical reservation")
		}
	}
	if !found {
		return errors.New("mutation finalization requires a newly attached output writer")
	}
	return nil
}
