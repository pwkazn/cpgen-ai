// Package config contains the closed local application configuration.
// Provider settings alone preserve the Fake workflow. A separate optional
// compiled workflow selector explicitly enables durable live stage assembly.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"cpgen/internal/domain"
	"go.yaml.in/yaml/v3"
)

const SchemaVersion = "cpgen.config/v1"

// FieldError retains the exact configuration field that failed validation.
// Callers can use errors.As rather than parsing human-readable messages.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

func field(name string, err error) error {
	if err == nil {
		return nil
	}
	return &FieldError{Field: name, Err: err}
}

type StorageConfig struct {
	StateRoot string `json:"state_root" yaml:"state_root"`
}

type SQLiteConfig struct {
	BusyTimeout time.Duration `json:"-" yaml:"-"`
	MaxReaders  int           `json:"max_readers" yaml:"max_readers"`
}

type RuntimeConfig struct {
	LockPollInterval    time.Duration `json:"-" yaml:"-"`
	ControlPollInterval time.Duration `json:"-" yaml:"-"`
	AccountingHeartbeat time.Duration `json:"-" yaml:"-"`
	CleanupWait         time.Duration `json:"-" yaml:"-"`
}

type FakeWorkflowConfig struct {
	Scenario string `json:"scenario" yaml:"scenario"`
}

// Short aliases make the public shape convenient for callers while retaining
// the explicit *Config names used in error messages and documentation.
type Storage = StorageConfig
type SQLite = SQLiteConfig
type Runtime = RuntimeConfig
type FakeWorkflow = FakeWorkflowConfig

// Paths are all derived from Storage.StateRoot.  They are not accepted from
// YAML and therefore cannot be pointed outside the private state directory.
type Paths struct {
	StateRoot  string `json:"state_root"`
	Database   string `json:"database"`
	Artifacts  string `json:"artifacts"`
	Runtime    string `json:"runtime"`
	Locks      string `json:"locks"`
	Temporary  string `json:"temporary"`
	Quarantine string `json:"quarantine"`
	Trash      string `json:"trash"`
	Work       string `json:"work"`
}

type Config struct {
	Storage      StorageConfig      `json:"storage" yaml:"storage"`
	SQLite       SQLiteConfig       `json:"sqlite" yaml:"sqlite"`
	Runtime      RuntimeConfig      `json:"runtime" yaml:"runtime"`
	FakeWorkflow FakeWorkflowConfig `json:"fake_workflow" yaml:"fake_workflow"`
	Paths        Paths              `json:"paths" yaml:"-"`
	LLM          *LLMConfig         `json:"llm,omitempty" yaml:"llm,omitempty"`
	Workflow     *WorkflowConfig    `json:"workflow,omitempty" yaml:"workflow,omitempty"`
	Similarity   *SimilarityConfig  `json:"similarity,omitempty" yaml:"similarity,omitempty"`
	Sandbox      *SandboxConfig     `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
}

// EffectiveConfig is the redacted, canonical configuration persisted with a
// run and printed by `config effective --redact`.
type EffectiveConfig struct {
	SchemaVersion string               `json:"schema_version"`
	Storage       EffectiveStorage     `json:"storage"`
	SQLite        EffectiveSQLite      `json:"sqlite"`
	Runtime       EffectiveRuntime     `json:"runtime"`
	FakeWorkflow  FakeWorkflowConfig   `json:"fake_workflow"`
	Paths         Paths                `json:"paths"`
	LLM           *EffectiveLLM        `json:"llm,omitempty"`
	Workflow      *WorkflowConfig      `json:"workflow,omitempty"`
	Similarity    *EffectiveSimilarity `json:"similarity,omitempty"`
	Sandbox       *SandboxConfig       `json:"sandbox,omitempty"`
}

type EffectiveStorage struct {
	StateRoot string `json:"state_root"`
}
type EffectiveSQLite struct {
	BusyTimeout string `json:"busy_timeout"`
	MaxReaders  int    `json:"max_readers"`
}
type EffectiveRuntime struct {
	LockPollInterval    string `json:"lock_poll_interval"`
	ControlPollInterval string `json:"control_poll_interval"`
	AccountingHeartbeat string `json:"accounting_heartbeat"`
	CleanupWait         string `json:"cleanup_wait"`
}

type rawConfig struct {
	Storage struct {
		StateRoot string `yaml:"state_root"`
	} `yaml:"storage"`
	SQLite struct {
		BusyTimeout string `yaml:"busy_timeout"`
		MaxReaders  int    `yaml:"max_readers"`
	} `yaml:"sqlite"`
	Runtime struct {
		LockPollInterval    string `yaml:"lock_poll_interval"`
		ControlPollInterval string `yaml:"control_poll_interval"`
		AccountingHeartbeat string `yaml:"accounting_heartbeat"`
		CleanupWait         string `yaml:"cleanup_wait"`
	} `yaml:"runtime"`
	FakeWorkflow struct {
		Scenario string `yaml:"scenario"`
	} `yaml:"fake_workflow"`
	LLM        *rawLLMConfig        `yaml:"llm"`
	Workflow   *rawWorkflowConfig   `yaml:"workflow"`
	Similarity *rawSimilarityConfig `yaml:"similarity"`
	Sandbox    *SandboxConfig       `yaml:"sandbox"`
}

var defaults = struct {
	busy, lock, control, heartbeat, cleanup time.Duration
}{5 * time.Second, 25 * time.Millisecond, 100 * time.Millisecond, time.Second, 10 * time.Second}

var scenarios = map[string]struct{}{
	"review": {}, "success": {}, "blocked": {}, "retry": {}, "failure": {}, "cancel": {},
	"artifact_review": {}, "sandbox_review": {}, "cache_review": {}, "mutation_review": {},
}

// Load reads one explicit configuration path. It never searches ambient
// directories or environment variables.
func Load(path string) (Config, error) {
	canonical, err := canonicalConfigPath(path)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(canonical)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", canonical, err)
	}
	return Decode(data)
}

func LoadFile(path string) (Config, error) { return Load(path) }

// Decode parses exactly one YAML document with duplicate and unknown key
// rejection. It is useful to callers that already resolved a config path.
func Decode(data []byte) (Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil {
		return Config{}, field("config", fmt.Errorf("decode YAML: %w", err))
	}
	if root.Kind == 0 {
		return Config{}, field("config", errors.New("config document is empty"))
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, field("config", errors.New("config contains trailing YAML document"))
		}
		return Config{}, field("config", fmt.Errorf("decode trailing YAML document: %w", err))
	}
	if err := inspectNode(&root, "", map[string]map[string]struct{}{
		"":              {"storage": {}, "sqlite": {}, "runtime": {}, "fake_workflow": {}, "llm": {}, "workflow": {}, "similarity": {}, "sandbox": {}},
		"storage":       {"state_root": {}},
		"sqlite":        {"busy_timeout": {}, "max_readers": {}},
		"runtime":       {"lock_poll_interval": {}, "control_poll_interval": {}, "accounting_heartbeat": {}, "cleanup_wait": {}},
		"fake_workflow": {"scenario": {}},
		"llm":           {"base_url": {}, "model": {}, "api_key_env": {}, "timeout": {}, "max_output_tokens": {}, "max_response_bytes": {}, "max_format_repairs": {}, "data_prompt_version": {}, "draft_retry_feedback_version": {}},
		"workflow":      {"revision": {}, "idea_count": {}, "llm_cost_upper_bound_micro_usd": {}, "similarity_cost_upper_bound_micro_usd": {}},
		"similarity":    {"endpoint": {}, "api_key_env": {}, "provider_identity": {}, "service_identity": {}, "timeout": {}, "max_response_bytes": {}, "limit": {}, "policy_ref": {}, "acceptance_threshold": {}, "rejection_threshold": {}, "minimum_hits": {}},
		"sandbox":       {"engine_endpoint": {}, "toolchain_lock_path": {}, "toolchain_lock_digest": {}},
	}); err != nil {
		return Config{}, err
	}
	var raw rawConfig
	if err := root.Decode(&raw); err != nil {
		return Config{}, field("config", fmt.Errorf("decode config fields: %w", err))
	}
	result := Config{Storage: StorageConfig{StateRoot: raw.Storage.StateRoot}, SQLite: SQLiteConfig{MaxReaders: raw.SQLite.MaxReaders}, FakeWorkflow: FakeWorkflowConfig{Scenario: raw.FakeWorkflow.Scenario}}
	result.Runtime = RuntimeConfig{}
	// Preserve the default only when max_readers is omitted. An explicitly
	// configured zero (or null) must reach Validate so it is rejected instead
	// of being silently rewritten to the default.
	if !hasYAMLField(&root, "sqlite", "max_readers") {
		result.SQLite.MaxReaders = 4
	}
	if result.FakeWorkflow.Scenario == "" {
		result.FakeWorkflow.Scenario = "review"
	}
	var err error
	result.SQLite.BusyTimeout, err = parseDuration(raw.SQLite.BusyTimeout, defaults.busy)
	if err != nil {
		return Config{}, field("sqlite.busy_timeout", err)
	}
	result.Runtime.LockPollInterval, err = parseDuration(raw.Runtime.LockPollInterval, defaults.lock)
	if err != nil {
		return Config{}, field("runtime.lock_poll_interval", err)
	}
	result.Runtime.ControlPollInterval, err = parseDuration(raw.Runtime.ControlPollInterval, defaults.control)
	if err != nil {
		return Config{}, field("runtime.control_poll_interval", err)
	}
	result.Runtime.AccountingHeartbeat, err = parseDuration(raw.Runtime.AccountingHeartbeat, defaults.heartbeat)
	if err != nil {
		return Config{}, field("runtime.accounting_heartbeat", err)
	}
	result.Runtime.CleanupWait, err = parseDuration(raw.Runtime.CleanupWait, defaults.cleanup)
	if err != nil {
		return Config{}, field("runtime.cleanup_wait", err)
	}
	result.LLM, err = decodeLLM(raw.LLM)
	if err != nil {
		return Config{}, err
	}
	result.Workflow, err = decodeWorkflow(raw.Workflow)
	if err != nil {
		return Config{}, err
	}
	result.Similarity, err = decodeSimilarity(raw.Similarity)
	if err != nil {
		return Config{}, err
	}
	result.Sandbox = effectiveSandbox(raw.Sandbox)
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func Parse(data []byte) (Config, error) { return Decode(data) }

func parseDuration(raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config is nil")
	}
	if c.LLM != nil {
		if err := c.LLM.Validate(); err != nil {
			return err
		}
	}
	if err := c.validateWorkflowDependencies(); err != nil {
		return err
	}
	root, err := canonicalStateRoot(c.Storage.StateRoot)
	if err != nil {
		return field("storage.state_root", err)
	}
	if c.SQLite.BusyTimeout <= 0 {
		return field("sqlite.busy_timeout", errors.New("must be positive"))
	}
	if c.SQLite.MaxReaders <= 0 {
		return field("sqlite.max_readers", errors.New("must be positive"))
	}
	if c.Runtime.LockPollInterval <= 0 {
		return field("runtime.lock_poll_interval", errors.New("must be positive"))
	}
	if c.Runtime.ControlPollInterval <= 0 {
		return field("runtime.control_poll_interval", errors.New("must be positive"))
	}
	if c.Runtime.AccountingHeartbeat <= 0 {
		return field("runtime.accounting_heartbeat", errors.New("must be positive"))
	}
	if c.Runtime.CleanupWait <= 0 {
		return field("runtime.cleanup_wait", errors.New("must be positive"))
	}
	if c.Runtime.ControlPollInterval < c.Runtime.LockPollInterval {
		return field("runtime.control_poll_interval", errors.New("must be at least lock_poll_interval"))
	}
	if c.SQLite.BusyTimeout < c.Runtime.LockPollInterval {
		return field("sqlite.busy_timeout", errors.New("must be at least lock_poll_interval"))
	}
	if c.Runtime.CleanupWait < c.Runtime.AccountingHeartbeat {
		return field("runtime.cleanup_wait", errors.New("must be at least accounting_heartbeat"))
	}
	if _, ok := scenarios[c.FakeWorkflow.Scenario]; !ok {
		return field("fake_workflow.scenario", fmt.Errorf("unsupported scenario %q", c.FakeWorkflow.Scenario))
	}
	for name, value := range map[string]string{"storage.state_root": c.Storage.StateRoot, "fake_workflow.scenario": c.FakeWorkflow.Scenario} {
		if strings.Contains(value, "${") || strings.Contains(value, "$env:") || strings.Contains(value, "$(") {
			return field(name, errors.New("environment interpolation is not permitted"))
		}
	}
	c.Paths = derivePaths(root)
	_, err = c.Effective()
	return err
}

func (c Config) Effective() ([]byte, error) {
	if c.LLM != nil {
		if err := c.LLM.Validate(); err != nil {
			return nil, err
		}
	}
	if err := c.validateWorkflowDependencies(); err != nil {
		return nil, err
	}
	if c.Paths.StateRoot == "" {
		root, err := canonicalStateRoot(c.Storage.StateRoot)
		if err != nil {
			return nil, field("storage.state_root", err)
		}
		c.Paths = derivePaths(root)
	}
	effective := EffectiveConfig{SchemaVersion: SchemaVersion,
		Storage:      EffectiveStorage{StateRoot: c.Paths.StateRoot},
		SQLite:       EffectiveSQLite{BusyTimeout: c.SQLite.BusyTimeout.String(), MaxReaders: c.SQLite.MaxReaders},
		Runtime:      EffectiveRuntime{LockPollInterval: c.Runtime.LockPollInterval.String(), ControlPollInterval: c.Runtime.ControlPollInterval.String(), AccountingHeartbeat: c.Runtime.AccountingHeartbeat.String(), CleanupWait: c.Runtime.CleanupWait.String()},
		FakeWorkflow: c.FakeWorkflow, Paths: c.Paths, LLM: effectiveLLM(c.LLM), Workflow: effectiveWorkflow(c.Workflow), Similarity: effectiveSimilarity(c.Similarity), Sandbox: effectiveSandbox(c.Sandbox)}
	encoded, err := json.Marshal(effective)
	if err != nil {
		return nil, err
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

func (c Config) EffectiveConfig() (EffectiveConfig, error) {
	if err := c.Validate(); err != nil {
		return EffectiveConfig{}, err
	}
	return EffectiveConfig{SchemaVersion: SchemaVersion, Storage: EffectiveStorage{StateRoot: c.Paths.StateRoot}, SQLite: EffectiveSQLite{BusyTimeout: c.SQLite.BusyTimeout.String(), MaxReaders: c.SQLite.MaxReaders}, Runtime: EffectiveRuntime{LockPollInterval: c.Runtime.LockPollInterval.String(), ControlPollInterval: c.Runtime.ControlPollInterval.String(), AccountingHeartbeat: c.Runtime.AccountingHeartbeat.String(), CleanupWait: c.Runtime.CleanupWait.String()}, FakeWorkflow: c.FakeWorkflow, Paths: c.Paths, LLM: effectiveLLM(c.LLM), Workflow: effectiveWorkflow(c.Workflow), Similarity: effectiveSimilarity(c.Similarity), Sandbox: effectiveSandbox(c.Sandbox)}, nil
}

func (c Config) EffectiveDigest() domain.Digest {
	// Public configuration fields (including the optional provider pointer)
	// can be edited after Decode. Hash the current snapshot, never stale data.
	encoded, err := c.Effective()
	if err != nil {
		return ""
	}
	return domain.SumBytes(encoded)
}

func (c Config) Digest() domain.Digest { return c.EffectiveDigest() }

func (c Config) RootPaths() Paths { return c.Paths }

func derivePaths(root string) Paths {
	artifacts := filepath.Join(root, "artifacts")
	return Paths{StateRoot: root, Database: filepath.Join(root, "workflow.db"), Artifacts: artifacts, Runtime: filepath.Join(root, "runtime"), Locks: filepath.Join(root, "runtime", "locks"), Temporary: filepath.Join(artifacts, "tmp"), Quarantine: filepath.Join(artifacts, "quarantine"), Trash: filepath.Join(artifacts, "trash"), Work: filepath.Join(root, "work")}
}

func canonicalConfigPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("config path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	if filepath.Clean(absolute) != absolute {
		return "", errors.New("config path must be canonical")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func canonicalStateRoot(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" || !filepath.IsAbs(raw) {
		return "", errors.New("must be an absolute path")
	}
	root := filepath.Clean(raw)
	if isFilesystemRoot(root) {
		return "", errors.New("filesystem root is not allowed")
	}
	if info, err := os.Lstat(root); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlink alias is not allowed")
		}
		if !info.IsDir() {
			return "", errors.New("state root is not a directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect state root: %w", err)
	}
	// An absent root may still have a symlink parent. Resolve the nearest
	// existing ancestor and reject aliases so the effective digest cannot
	// identify two filesystem locations as the same state root.
	ancestor := root
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			resolved, evalErr := filepath.EvalSymlinks(ancestor)
			if evalErr != nil {
				return "", fmt.Errorf("resolve state root ancestor: %w", evalErr)
			}
			if !samePath(filepath.Clean(resolved), filepath.Clean(ancestor)) {
				return "", errors.New("state root contains a symlink alias")
			}
			break
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
	}
	return root, nil
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func isFilesystemRoot(path string) bool {
	clean := filepath.Clean(path)
	return filepath.Dir(clean) == clean || (filepath.VolumeName(clean) != "" && strings.TrimPrefix(clean, filepath.VolumeName(clean)) == string(filepath.Separator))
}

func inspectNode(node *yaml.Node, path string, allowed map[string]map[string]struct{}) error {
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return field(pathOrRoot(path), errors.New("config document is malformed"))
		}
		return inspectNode(node.Content[0], path, allowed)
	}
	if node.Kind == yaml.AliasNode {
		return field(pathOrRoot(path), errors.New("YAML aliases are not permitted"))
	}
	if node.Kind != yaml.MappingNode {
		return field(pathOrRoot(path), errors.New("YAML document must be a mapping"))
	}
	keys := allowed[path]
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return field(pathOrRoot(path), errors.New("mapping keys must be strings"))
		}
		name := key.Value
		if _, ok := seen[name]; ok {
			return field(joinField(path, name), errors.New("duplicate key"))
		}
		seen[name] = struct{}{}
		if _, ok := keys[name]; !ok {
			return field(joinField(path, name), errors.New("unknown field"))
		}
		childPath := name
		if path != "" {
			childPath = path + "." + name
		}
		if value.Tag == "!!null" {
			return field(childPath, errors.New("null is not permitted"))
		}
		if value.Kind == yaml.AliasNode {
			return field(childPath, errors.New("YAML aliases are not permitted"))
		}
		if _, section := allowed[childPath]; section {
			if err := inspectNode(value, childPath, allowed); err != nil {
				return err
			}
		} else if value.Kind != yaml.ScalarNode {
			return field(childPath, errors.New("must be a scalar"))
		} else if path == "llm" {
			if err := inspectLLMScalar(name, value); err != nil {
				return field(childPath, err)
			}
		} else if path == "workflow" || path == "similarity" || path == "sandbox" {
			if err := inspectWorkflowScalar(path, name, value); err != nil {
				return field(childPath, err)
			}
		}
	}
	return nil
}

func hasYAMLField(node *yaml.Node, path ...string) bool {
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return false
		}
		node = node.Content[0]
	}
	for index, name := range path {
		if node.Kind != yaml.MappingNode {
			return false
		}
		found := false
		for position := 0; position+1 < len(node.Content); position += 2 {
			key, value := node.Content[position], node.Content[position+1]
			if key.Value != name {
				continue
			}
			if index == len(path)-1 {
				return true
			}
			node = value
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return false
}

func pathOrRoot(path string) string {
	if path == "" {
		return "config"
	}
	return path
}

func joinField(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
