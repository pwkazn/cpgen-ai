package config

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	DefaultLLMTimeout          = 30 * time.Second
	DefaultLLMMaxOutputTokens  = 4096
	DefaultLLMMaxResponseBytes = 1 << 20
	MaxLLMTimeout              = 10 * time.Minute
	MaxLLMOutputTokens         = 1 << 20
	MaxLLMResponseBytes        = 64 << 20
)

// LLMConfig is a credential-free provider policy. Nil Config.LLM means absent,
// retaining the pre-provider snapshot bytes. Retry is deliberately not a YAML
// option: durable physical dispatch owns retries, not the provider adapter.
type LLMConfig struct {
	BaseURL           string        `json:"base_url" yaml:"base_url"`
	Model             string        `json:"model" yaml:"model"`
	APIKeyEnv         string        `json:"api_key_env" yaml:"api_key_env"`
	Timeout           time.Duration `json:"-" yaml:"-"`
	MaxOutputTokens   int64         `json:"max_output_tokens" yaml:"max_output_tokens"`
	MaxResponseBytes  int64         `json:"max_response_bytes" yaml:"max_response_bytes"`
	MaxFormatRepairs  int64         `json:"max_format_repairs,omitempty" yaml:"max_format_repairs"`
	DataPromptVersion string        `json:"data_prompt_version,omitempty" yaml:"data_prompt_version,omitempty"`
}

type EffectiveLLM struct {
	BaseURL           string `json:"base_url"`
	Model             string `json:"model"`
	APIKeyEnv         string `json:"api_key_env"`
	Timeout           string `json:"timeout"`
	MaxOutputTokens   int64  `json:"max_output_tokens"`
	MaxResponseBytes  int64  `json:"max_response_bytes"`
	MaxFormatRepairs  int64  `json:"max_format_repairs,omitempty"`
	DataPromptVersion string `json:"data_prompt_version,omitempty"`
}

type rawLLMConfig struct {
	BaseURL           string  `yaml:"base_url"`
	Model             string  `yaml:"model"`
	APIKeyEnv         string  `yaml:"api_key_env"`
	Timeout           *string `yaml:"timeout"`
	MaxOutputTokens   *int64  `yaml:"max_output_tokens"`
	MaxResponseBytes  *int64  `yaml:"max_response_bytes"`
	MaxFormatRepairs  *int64  `yaml:"max_format_repairs"`
	DataPromptVersion string  `yaml:"data_prompt_version"`
}

func decodeLLM(raw *rawLLMConfig) (*LLMConfig, error) {
	if raw == nil {
		return nil, nil
	}
	c := &LLMConfig{BaseURL: raw.BaseURL, Model: raw.Model, APIKeyEnv: raw.APIKeyEnv, DataPromptVersion: raw.DataPromptVersion,
		Timeout: DefaultLLMTimeout, MaxOutputTokens: DefaultLLMMaxOutputTokens, MaxResponseBytes: DefaultLLMMaxResponseBytes}
	if raw.Timeout != nil {
		parsed, err := time.ParseDuration(*raw.Timeout)
		if err != nil {
			return nil, field("llm.timeout", errors.New("must be a valid duration"))
		}
		c.Timeout = parsed
	}
	if raw.MaxOutputTokens != nil {
		c.MaxOutputTokens = *raw.MaxOutputTokens
	}
	if raw.MaxResponseBytes != nil {
		c.MaxResponseBytes = *raw.MaxResponseBytes
	}
	if raw.MaxFormatRepairs != nil {
		c.MaxFormatRepairs = *raw.MaxFormatRepairs
	}
	return c, c.Validate()
}

// Validate performs no DNS lookup or credential lookup. Missing credentials
// are a dispatch-time concern; effective snapshots contain only their names.
// Errors never echo supplied values, including malformed URLs with secrets.
func (c LLMConfig) Validate() error {
	if !validLLMBaseURL(c.BaseURL) {
		return field("llm.base_url", errors.New("must be an HTTPS base URL with a valid non-local host, valid port, and no userinfo, query, fragment or ambiguous path"))
	}
	if !validLLMText(c.Model, 256) {
		return field("llm.model", errors.New("must be non-empty UTF-8 text of at most 256 bytes without surrounding whitespace, controls or interpolation"))
	}
	if len(c.APIKeyEnv) > 256 || !llmEnvName.MatchString(c.APIKeyEnv) {
		return field("llm.api_key_env", errors.New("must be an environment variable name of at most 256 bytes"))
	}
	if c.Timeout <= 0 || c.Timeout > MaxLLMTimeout {
		return field("llm.timeout", errors.New("must be positive and at most 10m"))
	}
	if c.MaxOutputTokens <= 0 || c.MaxOutputTokens > MaxLLMOutputTokens {
		return field("llm.max_output_tokens", errors.New("must be between 1 and 1048576"))
	}
	if c.MaxResponseBytes <= 0 || c.MaxResponseBytes > MaxLLMResponseBytes {
		return field("llm.max_response_bytes", errors.New("must be between 1 and 67108864"))
	}
	if c.MaxFormatRepairs < 0 || c.MaxFormatRepairs > 1 {
		return field("llm.max_format_repairs", errors.New("must be zero or one"))
	}
	if c.DataPromptVersion != "" && c.DataPromptVersion != "v3" && c.DataPromptVersion != "v5" {
		return field("llm.data_prompt_version", errors.New("must be empty, v3, or v5"))
	}
	return nil
}

var llmEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validLLMText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, unicode.IsControl) < 0 && !strings.Contains(value, "${") && !strings.Contains(value, "$(") && !strings.Contains(value, "$env:")
}

func validLLMBaseURL(raw string) bool {
	if !validLLMText(raw, 2048) || strings.ContainsAny(raw, "\\?#%") || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return false
	}
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return false
		}
	} else {
		if len(host) > 253 || !strings.Contains(host, ".") || strings.HasSuffix(host, ".localhost") {
			return false
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return false
			}
			for _, ch := range label {
				if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
					return false
				}
			}
		}
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func inspectLLMScalar(name string, node *yaml.Node) error {
	expected := "!!str"
	if name == "max_output_tokens" || name == "max_response_bytes" || name == "max_format_repairs" {
		expected = "!!int"
	}
	if node.Tag != expected {
		return errors.New("incorrect scalar type")
	}
	if expected == "!!int" {
		var value int64
		if err := node.Decode(&value); err != nil {
			return errors.New("must fit a signed 64-bit integer")
		}
	}
	return nil
}

func effectiveLLM(c *LLMConfig) *EffectiveLLM {
	if c == nil {
		return nil
	}
	return &EffectiveLLM{BaseURL: c.BaseURL, Model: c.Model, APIKeyEnv: c.APIKeyEnv,
		Timeout: c.Timeout.String(), MaxOutputTokens: c.MaxOutputTokens, MaxResponseBytes: c.MaxResponseBytes, MaxFormatRepairs: c.MaxFormatRepairs, DataPromptVersion: c.DataPromptVersion}
}
