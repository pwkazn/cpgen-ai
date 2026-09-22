package domain

import (
	"bytes"
	"encoding/json"
)

const ProgramContextSchema = "cpgen.program-context/v1"

// ProgramContext supplies semantics and sample inputs without unverified
// sample answers/explanations. The full canonical input remains the durable
// stage identity. This projection is separately bound to it, never decoded as
// a ProblemSpec or accepted as a replacement for the committed draft.
func programContext(raw []byte, err error, data bool) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // Preserve all seed/limit bits in the model projection.
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	solution := value
	if data {
		solution = value["solution_input"].(map[string]any)
	}
	problem := solution["problem"].(map[string]any)
	for _, item := range problem["samples"].([]any) {
		sample := item.(map[string]any)
		delete(sample, "output")
		delete(sample, "explanation")
	}
	return json.Marshal(struct {
		Schema  string         `json:"schema_version"`
		Source  Digest         `json:"source_input_digest"`
		Context map[string]any `json:"context"`
	}{ProgramContextSchema, SumBytes(raw), value})
}

func (v SolutionDraftInputV1) ProgramContextJSON() ([]byte, error) {
	raw, err := v.CanonicalJSON()
	return programContext(raw, err, false)
}

func (v DataDraftInputV1) ProgramContextJSON() ([]byte, error) {
	raw, err := v.CanonicalJSON()
	return programContext(raw, err, true)
}
