package port

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"cpgen/internal/domain"
)

// PromptVersionRef identifies an exact, immutable prompt version.  The
// template digest is over the UTF-8 template bytes and the schema digest is
// the digest of the provider-neutral output schema document.
type PromptVersionRef struct {
	Step           string          `json:"step"`
	Version        string          `json:"version"`
	TemplateDigest domain.Digest   `json:"template_digest"`
	OutputSchema   OutputSchemaRef `json:"output_schema"`
}

// TypedPromptRef is retained as a descriptive alias for callers that want to
// make the typed boundary explicit.
type TypedPromptRef = PromptVersionRef

func (r PromptVersionRef) Validate() error {
	if err := validateIdentifier("prompt step", r.Step); err != nil {
		return err
	}
	if err := validateIdentifier("prompt version", r.Version); err != nil {
		return err
	}
	if err := r.TemplateDigest.Validate(); err != nil {
		return fmt.Errorf("template digest: %w", err)
	}
	if err := r.OutputSchema.Validate(); err != nil {
		return fmt.Errorf("output schema: %w", err)
	}
	return nil
}

// PromptVersion is the registry's immutable definition. TemplateDigest is
// intentionally stored redundantly with Template so Validate can detect a
// definition whose bytes changed after registration.
type PromptVersion struct {
	Step               string               `json:"step"`
	Version            string               `json:"version"`
	Template           string               `json:"template"`
	TemplateDigest     domain.Digest        `json:"template_digest"`
	InputSchemaVersion domain.SchemaVersion `json:"input_schema_version,omitempty"`
	OutputSchema       OutputSchemaRef      `json:"output_schema"`
	MigrationPolicy    string               `json:"migration_policy,omitempty"`
}

// PromptDefinition and RegisteredPrompt are readable names for the same
// registry value. They are aliases so the public boundary stays one type.
type PromptDefinition = PromptVersion
type RegisteredPrompt = PromptVersion

func (p PromptVersion) Ref() PromptVersionRef {
	return PromptVersionRef{Step: p.Step, Version: p.Version, TemplateDigest: p.TemplateDigest, OutputSchema: p.OutputSchema}
}

func (p PromptVersion) Validate() error {
	if err := validateIdentifier("prompt step", p.Step); err != nil {
		return err
	}
	if err := validateIdentifier("prompt version", p.Version); err != nil {
		return err
	}
	if p.Template == "" || !utf8.ValidString(p.Template) {
		return errors.New("prompt template must be non-empty UTF-8")
	}
	if err := p.TemplateDigest.Validate(); err != nil {
		return fmt.Errorf("template digest: %w", err)
	}
	if got := domain.SumBytes([]byte(p.Template)); got != p.TemplateDigest {
		return errors.New("template digest does not match template bytes")
	}
	if p.InputSchemaVersion != "" {
		if err := p.InputSchemaVersion.Validate(); err != nil {
			return fmt.Errorf("input schema version: %w", err)
		}
	}
	if err := p.OutputSchema.Validate(); err != nil {
		return fmt.Errorf("output schema: %w", err)
	}
	if p.MigrationPolicy != "" && !validPolicy(p.MigrationPolicy) {
		return errors.New("invalid prompt migration policy")
	}
	return nil
}

func validPolicy(value string) bool {
	switch value {
	case "NONE", "BACKWARD_COMPATIBLE", "EXPLICIT_MIGRATION":
		return true
	default:
		return false
	}
}

// PromptRegistryError never includes template contents or caller-provided
// variables. It is safe to include in a stage result or log projection.
type PromptRegistryError struct {
	Code    PromptRegistryErrorCode
	Step    string
	Version string
}

type PromptRegistryErrorCode string

const (
	PromptDuplicateVersion  PromptRegistryErrorCode = "duplicate_version"
	PromptUnknownVersion    PromptRegistryErrorCode = "unknown_version"
	PromptInvalidDefinition PromptRegistryErrorCode = "invalid_definition"
)

func (e *PromptRegistryError) Error() string {
	if e == nil {
		return "prompt registry error"
	}
	switch e.Code {
	case PromptDuplicateVersion:
		return fmt.Sprintf("prompt registry: duplicate version for step %q", safeIdentifier(e.Step))
	case PromptUnknownVersion:
		return fmt.Sprintf("prompt registry: unknown version for step %q", safeIdentifier(e.Step))
	default:
		return "prompt registry: invalid definition"
	}
}

// PromptRegistry is safe for concurrent lookups. A registry is normally
// built once before a run; Register is provided for adapter setup and rejects
// duplicates rather than silently replacing a definition.
type PromptRegistry struct {
	mu      sync.RWMutex
	entries map[string]PromptVersion
}

func NewPromptRegistry(definitions ...PromptVersion) (*PromptRegistry, error) {
	r := &PromptRegistry{entries: make(map[string]PromptVersion, len(definitions))}
	for _, definition := range definitions {
		if err := r.Register(definition); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *PromptRegistry) Register(definition PromptVersion) error {
	if r == nil {
		return &PromptRegistryError{Code: PromptInvalidDefinition}
	}
	if err := definition.Validate(); err != nil {
		return &PromptRegistryError{Code: PromptInvalidDefinition, Step: definition.Step, Version: definition.Version}
	}
	key := promptKey(definition.Step, definition.Version)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]PromptVersion)
	}
	if _, exists := r.entries[key]; exists {
		return &PromptRegistryError{Code: PromptDuplicateVersion, Step: definition.Step, Version: definition.Version}
	}
	// Copying the value is enough: PromptVersion contains no mutable slices or
	// maps. The returned definition is also copied by value on lookup.
	r.entries[key] = definition
	return nil
}

func (r *PromptRegistry) Lookup(ref PromptVersionRef) (PromptVersion, error) {
	if r == nil {
		return PromptVersion{}, &PromptRegistryError{Code: PromptUnknownVersion, Step: ref.Step, Version: ref.Version}
	}
	if err := ref.Validate(); err != nil {
		return PromptVersion{}, &PromptRegistryError{Code: PromptInvalidDefinition, Step: ref.Step, Version: ref.Version}
	}
	r.mu.RLock()
	definition, exists := r.entries[promptKey(ref.Step, ref.Version)]
	r.mu.RUnlock()
	if !exists {
		return PromptVersion{}, &PromptRegistryError{Code: PromptUnknownVersion, Step: ref.Step, Version: ref.Version}
	}
	if definition.TemplateDigest != ref.TemplateDigest || definition.OutputSchema != ref.OutputSchema {
		return PromptVersion{}, &PromptRegistryError{Code: PromptUnknownVersion, Step: ref.Step, Version: ref.Version}
	}
	return definition, nil
}

// Resolve accepts a legacy PromptRef and checks its digest against the
// registered immutable template. It preserves GenerateRequest compatibility
// while giving new code a strict typed lookup.
func (r *PromptRegistry) Resolve(ref PromptRef) (PromptVersion, error) {
	digest := ref.TemplateDigest
	if digest == "" {
		digest = ref.Digest
	}
	return r.Lookup(PromptVersionRef{Step: ref.Step, Version: ref.Version, TemplateDigest: digest, OutputSchema: OutputSchemaRef{SchemaVersion: ref.SchemaVersion, Digest: ref.SchemaDigest}})
}

func (r *PromptRegistry) Versions() []PromptVersionRef {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	refs := make([]PromptVersionRef, 0, len(r.entries))
	for _, definition := range r.entries {
		refs = append(refs, definition.Ref())
	}
	r.mu.RUnlock()
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Step != refs[j].Step {
			return refs[i].Step < refs[j].Step
		}
		return refs[i].Version < refs[j].Version
	})
	return refs
}

func promptKey(step, version string) string { return step + "\x00" + version }

func validateIdentifier(name, value string) error {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func safeIdentifier(value string) string {
	if value == "" {
		return "<empty>"
	}
	if len(value) > 64 || !utf8.ValidString(value) {
		return "<invalid>"
	}
	return value
}

// PromptDefinitionDigest provides a stable digest for the complete public
// definition without hashing any secret or runtime variable.
func PromptDefinitionDigest(definition PromptVersion) (domain.Digest, error) {
	if err := definition.Validate(); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(struct {
		Step               string               `json:"step"`
		Version            string               `json:"version"`
		TemplateDigest     domain.Digest        `json:"template_digest"`
		InputSchemaVersion domain.SchemaVersion `json:"input_schema_version,omitempty"`
		OutputSchema       OutputSchemaRef      `json:"output_schema"`
		MigrationPolicy    string               `json:"migration_policy,omitempty"`
	}{definition.Step, definition.Version, definition.TemplateDigest, definition.InputSchemaVersion, definition.OutputSchema, definition.MigrationPolicy})
	if err != nil {
		return "", err
	}
	return domain.SumBytes(canonical), nil
}

// TemplateDigestFor is useful to construct a definition without allowing a
// caller to accidentally hash a different representation of the template.
func TemplateDigestFor(template string) domain.Digest {
	return domain.SumBytes([]byte(template))
}

// SHA256DigestForTemplate is the exact-byte variant used by PromptVersion.
func SHA256DigestForTemplate(template string) domain.Digest {
	return TemplateDigestFor(template)
}
