package port

import (
	"errors"
	"strings"
	"testing"

	"cpgen/internal/domain"
)

const testStructuredSchema = domain.SchemaVersion("cpgen.idea/v1")

type testStructuredValue struct {
	SchemaVersion string                  `json:"schema_version"`
	Title         string                  `json:"title"`
	Score         int                     `json:"score"`
	Metadata      *testStructuredMetadata `json:"metadata,omitempty"`
}

type testStructuredMetadata struct {
	Source string `json:"source"`
}

func TestStrictStructuredOutputBoundary(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		code StructuredOutputErrorCode
	}{
		{name: "duplicate top-level", raw: []byte(`{"schema_version":"cpgen.idea/v1","title":"safe","title":"secret"}`), code: StructuredOutputDuplicateField},
		{name: "duplicate nested", raw: []byte(`{"schema_version":"cpgen.idea/v1","title":"safe","nested":{"a":1,"a":2}}`), code: StructuredOutputDuplicateField},
		{name: "missing schema", raw: []byte(`{"title":"safe"}`), code: StructuredOutputSchemaMissing},
		{name: "wrong schema", raw: []byte(`{"schema_version":"cpgen.idea/v2","title":"safe"}`), code: StructuredOutputSchemaMismatch},
		{name: "invalid json", raw: []byte(`{"schema_version":"cpgen.idea/v1"`), code: StructuredOutputInvalidJSON},
		{name: "trailing json", raw: []byte(`{"schema_version":"cpgen.idea/v1"}{"secret":"do not log"}`), code: StructuredOutputInvalidJSON},
		{name: "invalid utf8", raw: []byte{'{', '"', 's', 'c', 'h', 'e', 'm', 'a', '_', 'v', 'e', 'r', 's', 'i', 'o', 'n', '"', ':', '"', 0xff, '"', '}'}, code: StructuredOutputInvalidUTF8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStructuredOutput(tc.raw, testStructuredSchema, 1024)
			var typed *StructuredOutputError
			if !errors.As(err, &typed) || typed.Code != tc.code {
				t.Fatalf("error = %T %v, want %s", err, err, tc.code)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("structured error leaked provider response content")
			}
		})
	}
	tooLarge := []byte(`{"schema_version":"cpgen.idea/v1","title":"` + strings.Repeat("x", 40) + `"}`)
	err := ValidateStructuredOutput(tooLarge, testStructuredSchema, 32)
	var typed *StructuredOutputError
	if !errors.As(err, &typed) || typed.Code != StructuredOutputTooLarge {
		t.Fatalf("size error = %T %v", err, err)
	}
}

func TestDecodeStructuredOutputRejectsUnknownFieldsAndProducesTypedValue(t *testing.T) {
	valid := []byte(`{"schema_version":"cpgen.idea/v1","title":"safe","score":7,"metadata":{"source":"fixture"}}`)
	value, err := DecodeStructured[testStructuredValue](valid, testStructuredSchema, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if value.Title != "safe" || value.Score != 7 {
		t.Fatalf("decoded value = %#v", value)
	}
	if value.Metadata == nil || value.Metadata.Source != "fixture" {
		t.Fatalf("decoded metadata = %#v", value.Metadata)
	}
	unknown := []byte(`{"schema_version":"cpgen.idea/v1","title":"safe","score":7,"secret":"do not log"}`)
	err = DecodeStructuredOutput(unknown, testStructuredSchema, 1024, &testStructuredValue{})
	var typed *StructuredOutputError
	if !errors.As(err, &typed) || typed.Code != StructuredOutputUnknownField {
		t.Fatalf("unknown field error = %T %v", err, err)
	}
	if strings.Contains(err.Error(), "do not log") {
		t.Fatal("unknown field error leaked value")
	}
	nestedUnknown := []byte(`{"schema_version":"cpgen.idea/v1","title":"safe","metadata":{"source":"fixture","secret":"do not log"}}`)
	err = DecodeStructuredOutput(nestedUnknown, testStructuredSchema, 1024, &testStructuredValue{})
	if !errors.As(err, &typed) || typed.Code != StructuredOutputUnknownField {
		t.Fatalf("nested unknown field error = %T %v", err, err)
	}
	if strings.Contains(err.Error(), "do not log") {
		t.Fatal("nested unknown field error leaked value")
	}
}

func TestBoundedRepairInputIsCanonicalAndBounded(t *testing.T) {
	failures := []*StructuredOutputError{
		{Code: StructuredOutputUnknownField, Path: "candidate.title"},
		{Code: StructuredOutputDuplicateField, Path: "candidate.title"},
		{Code: StructuredOutputUnknownField, Path: "candidate.title"},
	}
	input, err := NewBoundedRepairInput(testStructuredSchema, failures, []byte(`{"title":"safe"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.ErrorCodes) != 2 || input.ErrorCodes[0] != StructuredOutputDuplicateField || input.ErrorCodes[1] != StructuredOutputUnknownField {
		t.Fatalf("codes = %#v", input.ErrorCodes)
	}
	if len(input.FieldPaths) != 1 || input.FieldPaths[0] != "candidate.title" {
		t.Fatalf("paths = %#v", input.FieldPaths)
	}
	canonical, err := input.CanonicalJSON()
	if err != nil || len(canonical) > 1024 {
		t.Fatalf("canonical repair input = %s, err = %v", canonical, err)
	}
	if _, err := NewBoundedRepairInput(testStructuredSchema, failures, []byte(strings.Repeat("x", 20)), 8); err == nil {
		t.Fatal("oversized repair fragment was accepted")
	} else {
		var typed *StructuredOutputError
		if !errors.As(err, &typed) || typed.Code != StructuredOutputRepairTooLarge {
			t.Fatalf("repair size error = %T %v", err, err)
		}
	}
	if _, err := NewBoundedRepairInput(testStructuredSchema, []*StructuredOutputError{{Code: "provider-secret", Path: "title"}}, nil, 1024); err == nil {
		t.Fatal("unknown repair code was accepted")
	}
}
