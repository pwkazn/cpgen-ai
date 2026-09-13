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
	caseAlias := []byte(`{"schema_version":"cpgen.idea/v1","Title":"safe"}`)
	err = DecodeStructuredOutput(caseAlias, testStructuredSchema, 1024, &testStructuredValue{})
	if !errors.As(err, &typed) || typed.Code != StructuredOutputUnknownField {
		t.Fatalf("case alias error = %T %v", err, err)
	}
}

type constrainedStructuredValue struct {
	SchemaVersion string `json:"schema_version" required:"true"`
	Kind          string `json:"kind" enum:"IDEA,STATEMENT" required:"true"`
	Score         int    `json:"score" min:"0" max:"10"`
}

func TestDecodeStructuredOutputAppliesTypedRequiredEnumAndRangeRules(t *testing.T) {
	schema := OutputSchemaRef{SchemaVersion: testStructuredSchema, Digest: domain.SumBytes([]byte("schema"))}
	if err := RegisterDefaultTypedSchema[constrainedStructuredValue](schema, nil); err != nil {
		t.Fatal(err)
	}
	valid := []byte(`{"schema_version":"cpgen.idea/v1","kind":"IDEA","score":7}`)
	value, err := DecodeStructuredWithSchema[constrainedStructuredValue](valid, schema, 1024)
	if err != nil || value.Kind != "IDEA" || value.Score != 7 {
		t.Fatalf("valid constrained value = %#v, err = %v", value, err)
	}
	for name, raw := range map[string][]byte{
		"missing required": []byte(`{"schema_version":"cpgen.idea/v1","score":7}`),
		"invalid enum":     []byte(`{"schema_version":"cpgen.idea/v1","kind":"OTHER","score":7}`),
		"below range":      []byte(`{"schema_version":"cpgen.idea/v1","kind":"IDEA","score":-1}`),
		"above range":      []byte(`{"schema_version":"cpgen.idea/v1","kind":"IDEA","score":11}`),
	} {
		t.Run(name, func(t *testing.T) {
			var got constrainedStructuredValue
			err := DecodeStructuredOutput(raw, testStructuredSchema, 1024, &got)
			var typed *StructuredOutputError
			if !errors.As(err, &typed) || typed.Code != StructuredOutputTypeMismatch {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

func TestRequiredStructuredFieldsCheckPresenceNotZeroValue(t *testing.T) {
	type presenceValue struct {
		SchemaVersion string `json:"schema_version" required:"true"`
		Title         string `json:"title" required:"true"`
		Enabled       bool   `json:"enabled" required:"true"`
		Count         int    `json:"count" required:"true"`
	}
	raw := []byte(`{"schema_version":"cpgen.idea/v1","title":"","enabled":false,"count":0}`)
	var value presenceValue
	if err := DecodeStructuredOutput(raw, testStructuredSchema, 1024, &value); err != nil {
		t.Fatalf("explicit zero values rejected: %v", err)
	}
}

func TestSchemaValidatorRegistryBindsDigestAndFakeCanUseIt(t *testing.T) {
	type response struct {
		SchemaVersion string `json:"schema_version"`
		Title         string `json:"title" required:"true"`
	}
	firstSchema := OutputSchemaRef{SchemaVersion: testStructuredSchema, Digest: domain.SumBytes([]byte("first-schema"))}
	secondSchema := OutputSchemaRef{SchemaVersion: testStructuredSchema, Digest: domain.SumBytes([]byte("second-schema"))}
	registry, err := NewSchemaValidatorRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterTypedSchema[response](registry, firstSchema, nil); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"schema_version":"cpgen.idea/v1","title":"ok"}`)
	if _, err := DecodeStructuredWithSchemaRegistry[response](registry, raw, firstSchema, 1024); err != nil {
		t.Fatal(err)
	}
	if err := RegisterDefaultTypedSchema[response](firstSchema, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeStructuredWithSchemaRegistry[response](registry, raw, secondSchema, 1024); err == nil {
		t.Fatal("unregistered schema digest was accepted")
	} else {
		var typed *StructuredOutputError
		if !errors.As(err, &typed) || typed.Code != StructuredOutputSchemaUnbound {
			t.Fatalf("wrong digest error = %T %v", err, err)
		}
	}
	if _, err := DecodeStructuredWithSchema[response](raw, firstSchema, 1024); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedRepairInputIsCanonicalAndBounded(t *testing.T) {
	failures := []*StructuredOutputError{
		{Code: StructuredOutputUnknownField, Path: "candidate.title"},
		{Code: StructuredOutputDuplicateField, Path: "candidate.title"},
		{Code: StructuredOutputUnknownField, Path: "candidate.title"},
	}
	input, err := NewBoundedRepairInput(testStructuredSchema, failures, nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.ErrorCodes) != 2 || input.ErrorCodes[0] != StructuredOutputDuplicateField || input.ErrorCodes[1] != StructuredOutputUnknownField {
		t.Fatalf("codes = %#v", input.ErrorCodes)
	}
	if len(input.FieldPaths) != 0 {
		t.Fatalf("paths = %#v", input.FieldPaths)
	}
	withPaths, err := NewBoundedRepairInputWithPaths(testStructuredSchema, failures, []string{"candidate.title"}, 1024)
	if err != nil || len(withPaths.FieldPaths) != 1 || withPaths.FieldPaths[0] != "candidate.title" {
		t.Fatalf("schema-owned paths = %#v, err = %v", withPaths.FieldPaths, err)
	}
	canonical, err := input.CanonicalJSON()
	if err != nil || len(canonical) > 1024 {
		t.Fatalf("canonical repair input = %s, err = %v", canonical, err)
	}
	if _, err := NewBoundedRepairInput(testStructuredSchema, failures, []byte(strings.Repeat("x", 20)), 8); err == nil {
		t.Fatal("oversized repair fragment was accepted")
	} else {
		var typed *StructuredOutputError
		if !errors.As(err, &typed) || typed.Code != StructuredOutputRepairFragmentRejected {
			t.Fatalf("repair size error = %T %v", err, err)
		}
	}
	trusted, err := NewTrustedRepairFragment([]byte(`"safe"`))
	if err != nil {
		t.Fatal(err)
	}
	withFragment, err := NewBoundedRepairInputWithTrustedFragment(testStructuredSchema, failures, trusted, 1024)
	if err != nil || string(withFragment.InvalidFragment) != `"safe"` {
		t.Fatalf("trusted fragment = %s, err = %v", withFragment.InvalidFragment, err)
	}
	if _, err := NewTrustedRepairFragment([]byte(`{"secret":"do not disclose"}`)); err == nil {
		t.Fatal("object repair fragment was accepted")
	} else if strings.Contains(err.Error(), "do not disclose") {
		t.Fatal("repair error leaked fragment content")
	}
	if _, err := NewBoundedRepairInput(testStructuredSchema, []*StructuredOutputError{{Code: "provider-secret", Path: "title"}}, nil, 1024); err == nil {
		t.Fatal("unknown repair code was accepted")
	}
}

type nestedValidatedValue struct {
	SchemaVersion string               `json:"schema_version"`
	Child         nestedValidatedChild `json:"child"`
}

type nestedValidatedChild struct {
	Label string `json:"label" maxLength:"1"`
}

func (v *nestedValidatedChild) ValidateStructuredOutput() error {
	if v.Label != "é" {
		return errors.New("nested label invariant failed")
	}
	return nil
}

func TestDecodeStructuredOutputNormalizesUnicodeAndValidatesNestedTypes(t *testing.T) {
	raw := []byte("{\"schema_version\":\"cpgen.idea/v1\",\"child\":{\"label\":\"e\\u0301\"}}")
	var value nestedValidatedValue
	if err := DecodeStructuredOutput(raw, testStructuredSchema, 1024, &value); err != nil {
		t.Fatalf("nested normalized value rejected: %v", err)
	}
	if value.Child.Label != "é" {
		t.Fatalf("nested label was not NFC-normalized: %q", value.Child.Label)
	}
}
