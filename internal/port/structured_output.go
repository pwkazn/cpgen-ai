package port

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"cpgen/internal/domain"
	"golang.org/x/text/unicode/norm"
)

const (
	// DefaultStructuredOutputMaxBytes is deliberately small enough to keep a
	// malformed provider response out of logs and memory-heavy retry paths.
	DefaultStructuredOutputMaxBytes int64 = 1 << 20
	DefaultRepairInputMaxBytes      int64 = 4096
)

type StructuredOutputErrorCode string

const (
	StructuredOutputTooLarge               StructuredOutputErrorCode = "response_too_large"
	StructuredOutputInvalidUTF8            StructuredOutputErrorCode = "invalid_utf8"
	StructuredOutputInvalidJSON            StructuredOutputErrorCode = "invalid_json"
	StructuredOutputDuplicateField         StructuredOutputErrorCode = "duplicate_field"
	StructuredOutputUnknownField           StructuredOutputErrorCode = "unknown_field"
	StructuredOutputSchemaMismatch         StructuredOutputErrorCode = "schema_version_mismatch"
	StructuredOutputSchemaMissing          StructuredOutputErrorCode = "schema_version_missing"
	StructuredOutputTypeMismatch           StructuredOutputErrorCode = "type_mismatch"
	StructuredOutputRepairTooLarge         StructuredOutputErrorCode = "repair_input_too_large"
	StructuredOutputRepairFragmentRejected StructuredOutputErrorCode = "repair_fragment_rejected"
	StructuredOutputSchemaUnbound          StructuredOutputErrorCode = "schema_validator_unbound"
	StructuredOutputDomainInvalid          StructuredOutputErrorCode = "domain_validation_failed"
)

func (c StructuredOutputErrorCode) Valid() bool {
	switch c {
	case StructuredOutputTooLarge, StructuredOutputInvalidUTF8, StructuredOutputInvalidJSON,
		StructuredOutputDuplicateField, StructuredOutputUnknownField, StructuredOutputSchemaMismatch,
		StructuredOutputSchemaMissing, StructuredOutputTypeMismatch, StructuredOutputRepairTooLarge:
		return true
	case StructuredOutputRepairFragmentRejected:
		return true
	case StructuredOutputSchemaUnbound, StructuredOutputDomainInvalid:
		return true
	default:
		return false
	}
}

// FormatRepairable is deliberately narrower than Valid. Content semantics,
// oversize/encoding failures and missing local validators cannot consume the
// one JSON-format repair allowance.
func (c StructuredOutputErrorCode) FormatRepairable() bool {
	switch c {
	case StructuredOutputInvalidJSON, StructuredOutputDuplicateField, StructuredOutputUnknownField,
		StructuredOutputSchemaMismatch, StructuredOutputSchemaMissing, StructuredOutputTypeMismatch:
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
	case StructuredOutputRepairFragmentRejected:
		message = "structured repair fragment was not explicitly trusted"
	case StructuredOutputSchemaUnbound:
		message = "structured output schema has no trusted validator"
	case StructuredOutputDomainInvalid:
		message = "structured output violates domain constraints"
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
	if err := validateExactJSONAgainstType(raw, v.Elem().Type(), ""); err != nil {
		return err
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
	if err := validateTypedValue(v.Elem()); err != nil {
		return structuredError(StructuredOutputTypeMismatch, "")
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

// RepairInput is the only information a stage may disclose to a bounded
// repair call. It contains canonical codes and field paths plus an optional
// caller-selected minimum fragment. It is not a copy of the full response.
type RepairInput struct {
	SchemaVersion   domain.SchemaVersion        `json:"schema_version"`
	ErrorCodes      []StructuredOutputErrorCode `json:"error_codes"`
	FieldPaths      []string                    `json:"field_paths,omitempty"`
	InvalidFragment json.RawMessage             `json:"invalid_fragment,omitempty"`
}

// NewBoundedRepairInput intentionally drops all provider field paths and
// rejects raw fragments. Callers that have a schema-owned allow-list may use
// NewBoundedRepairInputWithPaths; callers that have a separately trusted
// scalar may use NewBoundedRepairInputWithTrustedFragment.
func NewBoundedRepairInput(schema domain.SchemaVersion, failures []*StructuredOutputError, fragment []byte, maxBytes int64) (RepairInput, error) {
	if len(fragment) > 0 {
		return RepairInput{}, structuredError(StructuredOutputRepairFragmentRejected, "invalid_fragment")
	}
	return newBoundedRepairInput(schema, failures, map[string]struct{}{}, nil, maxBytes)
}

// NewBoundedRepairInputWithPaths includes only paths explicitly supplied by
// the trusted schema owner. Provider-controlled keys are never copied merely
// because they happen to look like identifiers.
func NewBoundedRepairInputWithPaths(schema domain.SchemaVersion, failures []*StructuredOutputError, allowedPaths []string, maxBytes int64) (RepairInput, error) {
	allowed := make(map[string]struct{}, len(allowedPaths))
	for _, path := range allowedPaths {
		if !safeFieldPath(path) {
			return RepairInput{}, structuredError(StructuredOutputInvalidJSON, "field_paths")
		}
		allowed[path] = struct{}{}
	}
	return newBoundedRepairInput(schema, failures, allowed, nil, maxBytes)
}

// TrustedRepairFragment is constructed only from a single scalar JSON value
// selected by schema-aware code. Objects and arrays are rejected to prevent a
// complete model response from becoming a repair prompt.
type TrustedRepairFragment struct {
	raw json.RawMessage
}

func NewTrustedRepairFragment(raw []byte) (TrustedRepairFragment, error) {
	if len(raw) == 0 || len(raw) > 256 || !utf8.Valid(raw) || !json.Valid(raw) {
		return TrustedRepairFragment{}, structuredError(StructuredOutputRepairFragmentRejected, "invalid_fragment")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '{' || trimmed[0] == '[' {
		return TrustedRepairFragment{}, structuredError(StructuredOutputRepairFragmentRejected, "invalid_fragment")
	}
	return TrustedRepairFragment{raw: append(json.RawMessage(nil), trimmed...)}, nil
}

func (f TrustedRepairFragment) Bytes() []byte {
	return append([]byte(nil), f.raw...)
}

func NewBoundedRepairInputWithTrustedFragment(schema domain.SchemaVersion, failures []*StructuredOutputError, fragment TrustedRepairFragment, maxBytes int64) (RepairInput, error) {
	return newBoundedRepairInput(schema, failures, map[string]struct{}{}, fragment.raw, maxBytes)
}

func newBoundedRepairInput(schema domain.SchemaVersion, failures []*StructuredOutputError, allowedPaths map[string]struct{}, fragment []byte, maxBytes int64) (RepairInput, error) {
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
		if allowedPaths != nil {
			if _, allowed := allowedPaths[failure.Path]; !allowed {
				continue
			}
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
		trimmed := bytes.TrimSpace(r.InvalidFragment)
		if len(trimmed) > 256 {
			return &StructuredOutputError{Code: StructuredOutputRepairTooLarge, ExpectedBytes: 256, ActualBytes: int64(len(trimmed))}
		}
		if !utf8.Valid(trimmed) {
			return structuredError(StructuredOutputInvalidUTF8, "invalid_fragment")
		}
		if len(trimmed) == 0 || trimmed[0] == '{' || trimmed[0] == '[' {
			return structuredError(StructuredOutputRepairFragmentRejected, "invalid_fragment")
		}
		if !json.Valid(trimmed) {
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
		if part == "" || !safeIdentifierPart(part) {
			return false
		}
	}
	return true
}

func safeIdentifierPart(value string) bool {
	for index, r := range value {
		if index == 0 {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_') {
				return false
			}
			continue
		}
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
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

// validateExactJSONAgainstType closes encoding/json's case-insensitive field
// matching. It walks the JSON object tree against the target struct's exact
// JSON names before decoding, so "Title" cannot satisfy `json:"title"` and
// an unknown nested field cannot hide behind a valid outer object.
func validateExactJSONAgainstType(raw []byte, target reflect.Type, path string) error {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target == nil || target == reflect.TypeOf(json.RawMessage{}) || target == reflect.TypeOf(domain.Digest("")) {
		return nil
	}
	if target == reflect.TypeOf((*json.Unmarshaler)(nil)).Elem() || reflect.PointerTo(target).Implements(reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()) {
		return nil
	}
	if target.Kind() == reflect.Interface {
		return nil
	}
	switch target.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return structuredError(StructuredOutputTypeMismatch, path)
		}
		if object == nil {
			return structuredError(StructuredOutputTypeMismatch, path)
		}
		fields := exactJSONFields(target)
		present := make(map[string]struct{}, len(object))
		for name, value := range object {
			field, ok := fields[name]
			if !ok {
				return structuredError(StructuredOutputUnknownField, joinFieldPath(path, name))
			}
			present[name] = struct{}{}
			if err := validateExactJSONAgainstType(value, field.Type, joinFieldPath(path, name)); err != nil {
				return err
			}
		}
		for name, field := range fields {
			if isRequiredField(field) {
				if _, ok := present[name]; !ok {
					return structuredError(StructuredOutputTypeMismatch, joinFieldPath(path, name))
				}
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return structuredError(StructuredOutputTypeMismatch, path)
		}
		for index, value := range values {
			if target.Kind() == reflect.Array && index >= target.Len() {
				return structuredError(StructuredOutputTypeMismatch, path)
			}
			if err := validateExactJSONAgainstType(value, target.Elem(), fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case reflect.Map:
		// Map keys are data rather than schema field names. If the map has a
		// structured value, recurse into each value while preserving strict
		// checking for that value's struct fields.
		if target.Key().Kind() != reflect.String || target.Elem().Kind() == reflect.Interface {
			return nil
		}
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return structuredError(StructuredOutputTypeMismatch, path)
		}
		for name, value := range values {
			if err := validateExactJSONAgainstType(value, target.Elem(), joinFieldPath(path, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func exactJSONFields(target reflect.Type) map[string]reflect.StructField {
	fields := make(map[string]reflect.StructField)
	target = derefType(target)
	if target == nil || target.Kind() != reflect.Struct {
		return fields
	}
	for index := 0; index < target.NumField(); index++ {
		field := target.Field(index)
		if field.PkgPath != "" { // unexported
			continue
		}
		rawTag, hasJSONTag := field.Tag.Lookup("json")
		tag, options, hasOptions := strings.Cut(rawTag, ",")
		if hasJSONTag && tag == "" {
			tag = field.Name
		}
		if !hasJSONTag {
			tag = field.Name
		}
		if tag == "-" {
			continue
		}
		if field.Anonymous && !hasJSONTag {
			for name, promoted := range exactJSONFields(derefType(field.Type)) {
				fields[name] = promoted
			}
			continue
		}
		// Encoding/json ignores all options except known ones. Keeping this
		// assignment makes it explicit that `omitempty` is not a field name.
		_ = options
		_ = hasOptions
		if _, exists := fields[tag]; !exists {
			fields[tag] = field
		}
	}
	return fields
}

func derefType(target reflect.Type) reflect.Type {
	for target != nil && target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	return target
}

// StructuredValidator is an optional typed invariant hook. Domain values
// that already expose Validate() can be used directly; schema-specific
// output structs may implement ValidateStructuredOutput for cross-field,
// enum, and range checks.
type StructuredValidator interface {
	ValidateStructuredOutput() error
}

type ordinaryValidator interface {
	Validate() error
}

func validateTypedValue(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}
	// Normalize every decoded string before applying semantic validators. This
	// gives typed consumers one canonical Unicode representation while keeping
	// the raw provider bytes available for audit/debug boundaries.
	if err := normalizeStructuredValue(value); err != nil {
		return err
	}
	return validateTypedValueDeep(value)
}

func normalizeStructuredValue(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		raw := value.String()
		if !utf8.ValidString(raw) {
			return errors.New("structured field is not valid UTF-8")
		}
		if value.CanSet() {
			value.SetString(norm.NFC.String(raw))
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			if field.PkgPath != "" {
				continue
			}
			if err := normalizeStructuredValue(value.Field(index)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if err := normalizeStructuredValue(value.Index(index)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return nil
		}
		iter := value.MapRange()
		for iter.Next() {
			item := iter.Value()
			if item.Kind() == reflect.String {
				normalized := norm.NFC.String(item.String())
				if normalized != item.String() {
					value.SetMapIndex(iter.Key(), reflect.ValueOf(normalized).Convert(item.Type()))
				}
				continue
			}
			if err := normalizeStructuredValue(item); err != nil {
				return err
			}
		}
	case reflect.Interface:
		if !value.IsNil() {
			return normalizeStructuredValue(value.Elem())
		}
	}
	return nil
}

// validateTypedValueDeep invokes custom validators at every nested typed
// boundary, not only on the root response. This prevents a valid outer object
// from bypassing invariants on an embedded candidate/metadata value.
func validateTypedValueDeep(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			field := value.Type().Field(index)
			if field.PkgPath != "" {
				continue
			}
			current := value.Field(index)
			if err := validateTypedValueDeep(current); err != nil {
				return err
			}
		}
		if err := validateStructInvariants(value); err != nil {
			return err
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if err := validateTypedValueDeep(value.Index(index)); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			if err := validateTypedValueDeep(iter.Value()); err != nil {
				return err
			}
		}
	case reflect.Interface:
		if !value.IsNil() {
			return validateTypedValueDeep(value.Elem())
		}
	}
	return nil
}

func validateStructInvariants(value reflect.Value) error {
	validated := false
	if value.CanAddr() && value.Addr().CanInterface() {
		if validator, ok := value.Addr().Interface().(StructuredValidator); ok {
			validated = true
			if err := validator.ValidateStructuredOutput(); err != nil {
				return err
			}
		} else if validator, ok := value.Addr().Interface().(ordinaryValidator); ok {
			validated = true
			if err := validator.Validate(); err != nil {
				return err
			}
		}
	}
	if !validated && value.CanInterface() {
		if validator, ok := value.Interface().(StructuredValidator); ok {
			if err := validator.ValidateStructuredOutput(); err != nil {
				return err
			}
		} else if validator, ok := value.Interface().(ordinaryValidator); ok {
			if err := validator.Validate(); err != nil {
				return err
			}
		}
	}
	return validateTaggedValue(value)
}

// Tags are deliberately explicit: required:"true", enum:"a,b", min/max,
// and minLength/maxLength make the schema's semantic constraints executable
// without guessing whether an ordinary zero value is optional.
func validateTaggedValue(value reflect.Value) error {
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		return validateTaggedValue(value.Elem())
	}
	if value.Kind() != reflect.Struct {
		return nil
	}
	target := value.Type()
	for index := 0; index < target.NumField(); index++ {
		field := target.Field(index)
		if field.PkgPath != "" {
			continue
		}
		current := value.Field(index)
		tag := field.Tag.Get("validate")
		if enum := field.Tag.Get("enum"); enum != "" && !isAllowedEnum(current, strings.Split(enum, ",")) {
			return errors.New("structured field has an invalid enum value")
		}
		oneof := field.Tag.Get("oneof")
		if oneof == "" {
			oneof = validationOption(tag, "oneof")
		}
		if oneof != "" && !isAllowedEnum(current, strings.Fields(oneof)) {
			return errors.New("structured field has an invalid enum value")
		}
		if min, ok := numericTag(field, "min"); ok && !meetsMinimum(current, min) {
			return errors.New("structured field is below its minimum")
		}
		if max, ok := numericTag(field, "max"); ok && !meetsMaximum(current, max) {
			return errors.New("structured field exceeds its maximum")
		}
		if min, ok := numericTag(field, "minLength"); ok && lengthOf(current) < int(min) {
			return errors.New("structured field is shorter than its minimum")
		}
		if max, ok := numericTag(field, "maxLength"); ok && lengthOf(current) > int(max) {
			return errors.New("structured field exceeds its maximum length")
		}
		if err := validateTaggedValue(current); err != nil {
			return err
		}
	}
	return nil
}

func isRequiredField(field reflect.StructField) bool {
	tag := field.Tag.Get("validate")
	return field.Tag.Get("required") == "true" || strings.Contains(tag, "required")
}

func validationOption(tag, option string) string {
	for _, part := range strings.Split(tag, ",") {
		key, value, ok := strings.Cut(part, "=")
		if ok && strings.TrimSpace(key) == option {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isZeroValue(value reflect.Value) bool {
	if !value.IsValid() {
		return true
	}
	return value.IsZero()
}

func isAllowedEnum(value reflect.Value, allowed []string) bool {
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.String {
		return false
	}
	for _, candidate := range allowed {
		if value.String() == strings.TrimSpace(candidate) {
			return true
		}
	}
	return false
}

func numericTag(field reflect.StructField, name string) (float64, bool) {
	raw := field.Tag.Get(name)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func numericValue(value reflect.Value) (float64, bool) {
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0, false
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(value.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(value.Uint()), true
	case reflect.Float32, reflect.Float64:
		v := value.Float()
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	default:
		return 0, false
	}
}

func meetsMinimum(value reflect.Value, minimum float64) bool {
	actual, ok := numericValue(value)
	return ok && actual >= minimum
}

func meetsMaximum(value reflect.Value, maximum float64) bool {
	actual, ok := numericValue(value)
	return ok && actual <= maximum
}

func lengthOf(value reflect.Value) int {
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		return utf8.RuneCountInString(value.String())
	case reflect.Array, reflect.Slice, reflect.Map:
		return value.Len()
	default:
		return 0
	}
}
