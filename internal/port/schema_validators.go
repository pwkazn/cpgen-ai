package port

import (
	"errors"
	"fmt"
	"sync"

	"cpgen/internal/domain"
)

// StructuredSchemaValidator validates one complete provider response. The
// registry binds this callback to an exact OutputSchemaRef digest before it
// can be used, so a stale schema cannot select a different typed decoder.
type StructuredSchemaValidator func(raw []byte, schema OutputSchemaRef, maxBytes int64) error

type SchemaValidatorDefinition struct {
	Schema   OutputSchemaRef
	Validate StructuredSchemaValidator
}

type SchemaValidatorRegistry struct {
	mu      sync.RWMutex
	entries map[domain.Digest]SchemaValidatorDefinition
}

type SchemaValidatorErrorCode string

const (
	SchemaValidatorDuplicate SchemaValidatorErrorCode = "duplicate_schema_validator"
	SchemaValidatorInvalid   SchemaValidatorErrorCode = "invalid_schema_validator"
	SchemaValidatorUnknown   SchemaValidatorErrorCode = "unknown_schema_validator"
)

type SchemaValidatorError struct {
	Code   SchemaValidatorErrorCode
	Digest domain.Digest
}

func (e *SchemaValidatorError) Error() string {
	if e == nil {
		return "schema validator error"
	}
	switch e.Code {
	case SchemaValidatorDuplicate:
		return "schema validator registry: duplicate schema digest"
	case SchemaValidatorUnknown:
		return "schema validator registry: unknown schema digest"
	default:
		return "schema validator registry: invalid schema validator"
	}
}

func NewSchemaValidatorRegistry(definitions ...SchemaValidatorDefinition) (*SchemaValidatorRegistry, error) {
	registry := &SchemaValidatorRegistry{entries: make(map[domain.Digest]SchemaValidatorDefinition, len(definitions))}
	for _, definition := range definitions {
		if err := registry.Register(definition); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *SchemaValidatorRegistry) Register(definition SchemaValidatorDefinition) error {
	if r == nil || definition.Validate == nil {
		return &SchemaValidatorError{Code: SchemaValidatorInvalid, Digest: definition.Schema.Digest}
	}
	if err := definition.Schema.Validate(); err != nil {
		return &SchemaValidatorError{Code: SchemaValidatorInvalid, Digest: definition.Schema.Digest}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[domain.Digest]SchemaValidatorDefinition)
	}
	if _, exists := r.entries[definition.Schema.Digest]; exists {
		return &SchemaValidatorError{Code: SchemaValidatorDuplicate, Digest: definition.Schema.Digest}
	}
	r.entries[definition.Schema.Digest] = definition
	return nil
}

// ValidateBinding admits an exact schema reference before paid dispatch. It
// performs no decoding and never invokes a validator with a fabricated output.
func (r *SchemaValidatorRegistry) ValidateBinding(schema OutputSchemaRef) error {
	if r == nil || schema.Validate() != nil {
		return &StructuredOutputError{Code: StructuredOutputSchemaUnbound}
	}
	r.mu.RLock()
	definition, exists := r.entries[schema.Digest]
	r.mu.RUnlock()
	if !exists || definition.Schema != schema || definition.Validate == nil {
		return &StructuredOutputError{Code: StructuredOutputSchemaUnbound}
	}
	return nil
}

// Validate first checks the generic response boundary, then dispatches to the
// validator selected by the exact schema digest. Non-typed callback failures
// are sanitized before leaving this package.
func (r *SchemaValidatorRegistry) Validate(raw []byte, schema OutputSchemaRef, maxBytes int64) error {
	if r == nil {
		return &StructuredOutputError{Code: StructuredOutputSchemaUnbound}
	}
	if err := schema.Validate(); err != nil {
		return &StructuredOutputError{Code: StructuredOutputSchemaMismatch, Path: "schema_version"}
	}
	if err := ValidateStructuredOutput(raw, schema.SchemaVersion, maxBytes); err != nil {
		return err
	}
	r.mu.RLock()
	definition, exists := r.entries[schema.Digest]
	r.mu.RUnlock()
	if !exists || definition.Schema != schema {
		return &StructuredOutputError{Code: StructuredOutputSchemaUnbound}
	}
	if err := definition.Validate(raw, schema, maxBytes); err != nil {
		var typed *StructuredOutputError
		if errors.As(err, &typed) {
			return err
		}
		return &StructuredOutputError{Code: StructuredOutputDomainInvalid}
	}
	return nil
}

// RegisterTypedSchema installs strict reflection decoding plus an optional
// domain validator. The output schema digest is the trust anchor for the
// validator; callers cannot swap it by changing only a schema version.
func RegisterTypedSchema[T any](registry *SchemaValidatorRegistry, schema OutputSchemaRef, validate func(*T) error) error {
	if validate == nil {
		validate = func(*T) error { return nil }
	}
	return registry.Register(SchemaValidatorDefinition{
		Schema: schema,
		Validate: func(raw []byte, expected OutputSchemaRef, maxBytes int64) error {
			var value T
			if err := DecodeStructuredOutput(raw, expected.SchemaVersion, maxBytes, &value); err != nil {
				return err
			}
			return validate(&value)
		},
	})
}

// DecodeStructuredWithSchemaRegistry selects a trusted typed validator using
// the complete schema reference and then decodes the value for the caller.
func DecodeStructuredWithSchemaRegistry[T any](registry *SchemaValidatorRegistry, raw []byte, schema OutputSchemaRef, maxBytes int64) (T, error) {
	var value T
	if err := registry.Validate(raw, schema, maxBytes); err != nil {
		return value, err
	}
	if err := DecodeStructuredOutput(raw, schema.SchemaVersion, maxBytes, &value); err != nil {
		return value, err
	}
	return value, nil
}

var defaultSchemaValidators = &SchemaValidatorRegistry{entries: make(map[domain.Digest]SchemaValidatorDefinition)}

// DefaultSchemaValidatorRegistry is the process registry for schemas bundled
// with the local application. Applications should register immutable schema
// definitions during startup, before accepting model work.
func DefaultSchemaValidatorRegistry() *SchemaValidatorRegistry { return defaultSchemaValidators }

func RegisterDefaultTypedSchema[T any](schema OutputSchemaRef, validate func(*T) error) error {
	return RegisterTypedSchema(defaultSchemaValidators, schema, validate)
}

// DecodeStructuredWithSchema retains the original convenience API while
// requiring the schema digest to be registered to a trusted typed validator.
func DecodeStructuredWithSchema[T any](raw []byte, schema OutputSchemaRef, maxBytes int64) (T, error) {
	return DecodeStructuredWithSchemaRegistry[T](defaultSchemaValidators, raw, schema, maxBytes)
}

func ValidateStructuredWithSchema(raw []byte, schema OutputSchemaRef, maxBytes int64) error {
	return defaultSchemaValidators.Validate(raw, schema, maxBytes)
}

func (e SchemaValidatorErrorCode) String() string { return fmt.Sprint(string(e)) }
