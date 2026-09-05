package application

import (
	"context"
	"errors"
	"testing"

	"cpgen/internal/domain"
)

func TestCacheVerificationErrorIsTyped(t *testing.T) {
	key := domain.CacheKey{Digest: domain.SumBytes([]byte("key")), Kind: "statement"}
	err := &CacheVerificationError{Key: key, Cause: errors.New("missing")}
	if !errors.Is(err, err.Cause) {
		t.Fatal("verification cause is not unwrap-able")
	}
	if err.Error() == "" {
		t.Fatal("verification error has no message")
	}
	_ = context.Background()
}
