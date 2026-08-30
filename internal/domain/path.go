package domain

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// SafeRelPath is a canonical, slash-separated relative path.
type SafeRelPath string

func ParseSafeRelPath(value string) (SafeRelPath, error) {
	p := SafeRelPath(value)
	if err := p.Validate(); err != nil {
		return "", err
	}
	return p, nil
}

func (p SafeRelPath) Validate() error {
	value := string(p)
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("invalid safe relative path %q", value)
	}
	if strings.Contains(value, `\`) || strings.Contains(value, ":") || strings.HasPrefix(value, "/") {
		return fmt.Errorf("path is not a canonical relative path %q", value)
	}
	if path.Clean(value) != value || value == "." {
		return fmt.Errorf("path is not canonical %q", value)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("unsafe path segment in %q", value)
		}
	}
	return nil
}

func (p *SafeRelPath) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode safe relative path: %w", err)
	}
	parsed, err := ParseSafeRelPath(raw)
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}
