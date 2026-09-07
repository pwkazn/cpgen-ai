package port

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"cpgen/internal/domain"
)

const (
	// DefaultStructuredOutputMaxBytes is deliberately small enough to keep a
	// malformed provider response out of logs and memory-heavy retry paths.
	DefaultStructuredOutputMaxBytes int64 = 1 << 20
	DefaultRepairInputMaxBytes      int64 = 4096
)

type StructuredOutputErrorCode string

const (
	StructuredOutputTooLarge       StructuredOutputErrorCode = "response_too_large"
	StructuredOutputInvalidUTF8    StructuredOutputErrorCode = "invalid_utf8"
	StructuredOutputInvalidJSON    StructuredOutputErrorCode = "invalid_json"
	StructuredOutputDuplicateField StructuredOutputErrorCode = "duplicate_field"
	StructuredOutputUnknownField   StructuredOutputErrorCode = "unknown_field"
	StructuredOutputSchemaMismatch StructuredOutputErrorCode = "schema_version_mismatch"
	StructuredOutputSchemaMissing  StructuredOutputErrorCode = "schema_version_missing"
	StructuredOutputTypeMismatch   StructuredOutputErrorCode = "type_mismatch"
	StructuredOutputRepairTooLarge StructuredOutputErrorCode = "repair_input_too_large"
)

func (c StructuredOutputErrorCode) Valid() bool {
	switch c {
	case StructuredOutputTooLarge, StructuredOutputInvalidUTF8, StructuredOutputInvalidJSON,
		StructuredOutputDuplicateField, StructuredOutputUnknownField, StructuredOutputSchemaMismatch,
		StructuredOutputSchemaMissing, StructuredOutputTypeMismatch, StructuredOutputRepairTooLarge:
		return true
	default:
		return false
	}
}

// StructuredOutputError is intentionally a small, sanitized error. It never
// stores raw provider output or parser messages (which can contain prompt
// data). Path is a schema field name, not an arbitrary response fragment.
type StructuredOutputError struct {
	Code          StructuredOutputErrorCode
	Path          string
	ExpectedBytes int64
	ActualBytes   int64
}

func (e *StructuredOutputError) Error() string {
	if e == nil {
		return "structured output validation failed"
	}
	message := "structured output validation failed"
	switch e.Code {
	case StructuredOutputTooLarge:
		message = "structured output exceeds configured size limit"
	case StructuredOutputInvalidUTF8:
		message = "structured output is not valid UTF-8"
	case StructuredOutputInvalidJSON:
		message = "structured output is not valid JSON"
	case StructuredOutputDuplicateField:
		message = "structured output contains a duplicate field"
	case StructuredOutputUnknownField:
		message = "structured output contains an unknown field"
	case StructuredOutputSchemaMismatch:
		message = "structured output schema version is not accepted"
	case StructuredOutputSchemaMissing:
		message = "structured output schema version is missing"
	case StructuredOutputTypeMismatch:
		message = "structured output has an invalid field type"
	case StructuredOutputRepairTooLarge:
		message = "structured repair input exceeds configured size limit"
	}
	if e.Path != "" && safeFieldPath(e.Path) {
		return message + " at field " + e.Path
	}
	return message
}

func (e *StructuredOutputError) Is(target error) bool {
	other, ok := target.(*StructuredOutputError)
	return ok && e != nil && other != nil && e.Code == other.Code
}

func structuredError(code StructuredOutputErrorCode, path string) error {
	if !safeFieldPath(path) {
		path = ""
	}
	return &StructuredOutputError{Code: code, Path: path}
}

// ValidateStructuredOutput validates the provider boundary without decoding
// into a concrete Go value. The response must be a JSON object containing an
// exact schema_version field.
func ValidateStructuredOutput(raw []byte, expected domain.SchemaVersion, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = DefaultStructuredOutputMaxBytes
	}
	if int64(len(raw)) > maxBytes {
		return &StructuredOutputError{Code: StructuredOutputTooLarge, ExpectedBytes: maxBytes, ActualBytes: int64(len(raw))}
	}
	if !utf8.Valid(raw) {
		return structuredError(StructuredOutputInvalidUTF8, "")
	}
	if err := expected.Validate(); err != nil {
		return structuredError(StructuredOutputSchemaMismatch, "schema_version")
	}
	if err := scanJSONDocument(raw); err != nil {
		return structuredError(errorCodeForScan(err), scanPathForScan(err))
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return structuredError(StructuredOutputTypeMismatch, "")
	}
	schemaRaw, ok := object["schema_version"]
	if !ok {
		return structuredError(StructuredOutputSchemaMissing, "schema_version")
	}
	var actual string
	if err := json.Unmarshal(schemaRaw, &actual); err != nil {
		return structuredError(StructuredOutputTypeMismatch, "schema_version")
	}
	if domain.SchemaVersion(actual) != expected {
		return structuredError(StructuredOutputSchemaMismatch, "schema_version")
	}
	return nil
}

// DecodeStructuredOutput performs the provider boundary checks and then
// decodes into dst with unknown fields rejected at every ordinary struct
// boundary. dst must be a non-nil pointer. A custom UnmarshalJSON method can
// still opt into its own semantics; callers should keep response types plain
// data structs at this boundary.
func DecodeStructuredOutput(raw []byte, expected domain.SchemaVersion, maxBytes int64, dst any) error {
	if err := ValidateStructuredOutput(raw, expected, maxBytes); err != nil {
		return err
	}
	if dst == nil {
		return nil
	}
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return structuredError(StructuredOutputTypeMismatch, "")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return structuredError(StructuredOutputUnknownField, "")
		}
		return structuredError(StructuredOutputTypeMismatch, "")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return structuredError(StructuredOutputInvalidJSON, "")
	}
	return nil
}

// DecodeStructured is a generic convenience wrapper for typed response
// structs. It is intentionally separate from MeteredLLM so existing callers
// using GenerateResponse remain source-compatible.
func DecodeStructured[T any](raw []byte, expected domain.SchemaVersion, maxBytes int64) (T, error) {
	var value T
	if err := DecodeStructuredOutput(raw, expected, maxBytes, &value); err != nil {
		return value, err
	}
	return value, nil
}

// DecodeStructuredWithSchema is the OutputSchemaRef variant used by model
// adapters. The schema digest is checked as a typed reference, while the
// response itself carries and proves the schema version.
func DecodeStructuredWithSchema[T any](raw []byte, schema OutputSchemaRef, maxBytes int64) (T, error) {
	var value T
	if err := schema.Validate(); err != nil {
		return value, structuredError(StructuredOutputSchemaMismatch, "schema_version")
	}
	if err := DecodeStructuredOutput(raw, schema.SchemaVersion, maxBytes, &value); err != nil {
		return value, err
	}
	return value, nil
}

// RepairInput is the only information a stage may disclose to a bounded
// repair call. It contains canonical codes and field paths plus an optional
// caller-selected minimum fragment. It is not a copy of the full response.
type RepairInput struct {
	SchemaVersion   domain.SchemaVersion        `json:"schema_version"`
	ErrorCodes      []StructuredOutputErrorCode `json:"error_codes"`
	FieldPaths      []string                    `json:"field_paths,omitempty"`
	InvalidFragment json.RawMessage             `json:"invalid_fragment,omitempty"`
}

func NewBoundedRepairInput(schema domain.SchemaVersion, failures []*StructuredOutputError, fragment []byte, maxBytes int64) (RepairInput, error) {
	var input RepairInput
	if err := schema.Validate(); err != nil {
		return input, structuredError(StructuredOutputSchemaMismatch, "schema_version")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultRepairInputMaxBytes
	}
	input.SchemaVersion = schema
	seenCodes := make(map[StructuredOutputErrorCode]struct{}, len(failures))
	seenPaths := make(map[string]struct{}, len(failures))
	for _, failure := range failures {
		if failure == nil || failure.Code == "" {
			continue
		}
		if !failure.Code.Valid() {
			return RepairInput{}, structuredError(StructuredOutputInvalidJSON, "error_codes")
		}
		if _, ok := seenCodes[failure.Code]; !ok {
			input.ErrorCodes = append(input.ErrorCodes, failure.Code)
			seenCodes[failure.Code] = struct{}{}
		}
		if safeFieldPath(failure.Path) {
			if _, ok := seenPaths[failure.Path]; !ok {
				input.FieldPaths = append(input.FieldPaths, failure.Path)
				seenPaths[failure.Path] = struct{}{}
			}
		}
	}
	if len(input.ErrorCodes) == 0 {
		return RepairInput{}, structuredError(StructuredOutputInvalidJSON, "")
	}
	sort.Slice(input.ErrorCodes, func(i, j int) bool { return input.ErrorCodes[i] < input.ErrorCodes[j] })
	sort.Strings(input.FieldPaths)
	if len(fragment) > 0 {
		if !utf8.Valid(fragment) || int64(len(fragment)) > maxBytes {
			return RepairInput{}, &StructuredOutputError{Code: StructuredOutputRepairTooLarge, ExpectedBytes: maxBytes, ActualBytes: int64(len(fragment))}
		}
		input.InvalidFragment = append(json.RawMessage(nil), fragment...)
	}
	canonical, err := input.CanonicalJSON()
	if err != nil {
		return RepairInput{}, err
	}
	if int64(len(canonical)) > maxBytes {
		return RepairInput{}, &StructuredOutputError{Code: StructuredOutputRepairTooLarge, ExpectedBytes: maxBytes, ActualBytes: int64(len(canonical))}
	}
	return input, nil
}

func (r RepairInput) Validate(maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = DefaultRepairInputMaxBytes
	}
	if err := r.SchemaVersion.Validate(); err != nil {
		return structuredError(StructuredOutputSchemaMismatch, "schema_version")
	}
	if len(r.ErrorCodes) == 0 {
		return structuredError(StructuredOutputInvalidJSON, "error_codes")
	}
	for _, code := range r.ErrorCodes {
		if !code.Valid() {
			return structuredError(StructuredOutputInvalidJSON, "error_codes")
		}
	}
	for index := 1; index < len(r.ErrorCodes); index++ {
		if r.ErrorCodes[index-1] >= r.ErrorCodes[index] {
			return structuredError(StructuredOutputInvalidJSON, "error_codes")
		}
	}
	for _, path := range r.FieldPaths {
		if !safeFieldPath(path) {
			return structuredError(StructuredOutputInvalidJSON, "field_paths")
		}
	}
	for index := 1; index < len(r.FieldPaths); index++ {
		if r.FieldPaths[index-1] >= r.FieldPaths[index] {
			return structuredError(StructuredOutputInvalidJSON, "field_paths")
		}
	}
	if len(r.InvalidFragment) > 0 {
		if !utf8.Valid(r.InvalidFragment) {
			return structuredError(StructuredOutputInvalidUTF8, "invalid_fragment")
		}
		if !json.Valid(r.InvalidFragment) {
			return structuredError(StructuredOutputInvalidJSON, "invalid_fragment")
		}
	}
	canonical, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if int64(len(canonical)) > maxBytes {
		return &StructuredOutputError{Code: StructuredOutputRepairTooLarge, ExpectedBytes: maxBytes, ActualBytes: int64(len(canonical))}
	}
	return nil
}

func (r RepairInput) CanonicalJSON() ([]byte, error) {
	if err := r.Validate(DefaultRepairInputMaxBytes); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

func safeFieldPath(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if part == "" || strings.ContainsAny(part, "[]{}\"'") {
			return false
		}
	}
	return true
}

type scanError struct {
	code StructuredOutputErrorCode
	path string
}

func (e *scanError) Error() string { return string(e.code) }

func errorCodeForScan(err error) StructuredOutputErrorCode {
	var scan *scanError
	if errors.As(err, &scan) && scan.code != "" {
		return scan.code
	}
	return StructuredOutputInvalidJSON
}

func scanPathForScan(err error) string {
	var scan *scanError
	if errors.As(err, &scan) && safeFieldPath(scan.path) {
		return scan.path
	}
	return ""
}

func scanJSONDocument(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, ""); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing json")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch delimiter := token.(type) {
	case json.Delim:
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("invalid json key")
				}
				if _, exists := seen[key]; exists {
					return &scanError{code: StructuredOutputDuplicateField, path: joinFieldPath(path, key)}
				}
				seen[key] = struct{}{}
				if err := scanJSONValue(decoder, joinFieldPath(path, key)); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("invalid json object")
			}
		case '[':
			index := 0
			for decoder.More() {
				if err := scanJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
					return err
				}
				index++
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("invalid json array")
			}
		default:
			return errors.New("invalid json delimiter")
		}
	}
	return nil
}

func joinFieldPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}
