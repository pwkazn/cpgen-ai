package judge

import (
	"bytes"
	_ "embed"
	"encoding/json"

	"cpgen/internal/domain"
)

const ExactTokenComparisonV1 = "exact-tokens-v1"

//go:embed checker/exact_tokens.cpp
var exactTokenCheckerSource string

// ExactTokenCheckerSource returns an independent copy of the standard-library
// C++ checker. No arguments selects the fixed Sandbox input mounts; exported
// callers may supply input, output and answer paths in testlib order.
func ExactTokenCheckerSource() []byte { return []byte(exactTokenCheckerSource) }

// ExactTokenDigest preserves token bytes (including malformed UTF-8), ignores
// Unicode whitespace and makes neither numeric nor case normalization claims.
func ExactTokenDigest(raw []byte) domain.Digest {
	encoded, _ := json.Marshal(bytes.Fields(raw))
	return domain.SumBytes(encoded)
}
