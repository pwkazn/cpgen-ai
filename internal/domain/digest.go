package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const digestPrefix = "sha256:"

// Digest is a lowercase SHA-256 content digest in sha256:<hex> form.
type Digest string

func ParseDigest(value string) (Digest, error) {
	digest := Digest(value)
	if err := digest.Validate(); err != nil {
		return "", err
	}
	return digest, nil
}

func SumBytes(value []byte) Digest {
	sum := sha256.Sum256(value)
	return Digest(digestPrefix + hex.EncodeToString(sum[:]))
}

func SumReader(reader io.Reader) (Digest, int64, error) {
	hash := sha256.New()
	size, err := io.Copy(hash, reader)
	if err != nil {
		return "", size, fmt.Errorf("hash content: %w", err)
	}
	return Digest(digestPrefix + hex.EncodeToString(hash.Sum(nil))), size, nil
}

func (d Digest) Validate() error {
	raw := string(d)
	if !strings.HasPrefix(raw, digestPrefix) || len(raw) != len(digestPrefix)+sha256.Size*2 {
		return fmt.Errorf("invalid SHA-256 digest %q", raw)
	}
	hexPart := strings.TrimPrefix(raw, digestPrefix)
	if strings.ToLower(hexPart) != hexPart {
		return fmt.Errorf("digest must use lowercase hexadecimal")
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return fmt.Errorf("invalid SHA-256 digest: %w", err)
	}
	return nil
}

func (d *Digest) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode digest: %w", err)
	}
	parsed, err := ParseDigest(raw)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
