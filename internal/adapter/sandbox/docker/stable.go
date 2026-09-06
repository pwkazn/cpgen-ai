package docker

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/watchdog"
)

func callRecordForAttempt(id domain.AttemptCallID) domain.CallRecordID {
	raw := string(id)
	if len(raw) > len("call_") && raw[:len("call_")] == "call_" {
		raw = raw[len("call_"):]
	}
	return domain.CallRecordID("callrec_" + raw)
}

// stableToken derives command identity from the sealed execution/resource
// identity. A retry after a process crash therefore addresses the same
// durable command instead of creating a fresh idempotency key.
func stableToken(prefix string, values ...string) string {
	h := sha256.New()
	for _, value := range values {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(value)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(value))
	}
	sum := h.Sum(nil)
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func stableSandboxID(prefix string, values ...string) string {
	return stableToken(prefix, values...)
}

func stableSandboxKey(prefix string, values ...string) string {
	return stableToken(prefix, values...)
}

// Lifecycle timestamps are deterministic within an operation. The small
// phase offset preserves ordering while avoiding wall-clock drift on replay.
func stableLifecycleTime(base time.Time, phase string) time.Time {
	offset := map[string]time.Duration{
		"prepare": 0, "arm": time.Second, "creating": 2 * time.Second,
		"precreate": 3 * time.Second, "dispatch": 4 * time.Second,
		"sent": 5 * time.Second, "complete": 6 * time.Second,
		"unknown": 6 * time.Second,
		"started": 7 * time.Second, "cleanup": 8 * time.Second,
		"stop": 9 * time.Second, "clean": 10 * time.Second,
		"interrupt": 11 * time.Second, "finish": 12 * time.Second,
	}[phase]
	return base.UTC().Add(offset)
}

func stableControlNonce(record watchdog.ControlRecord) string {
	return stableHex(string(record.Plan.PlanDigest), record.LogicalOperationID, string(record.EngineIdentityDigest))
}

func stableHex(values ...string) string {
	h := sha256.New()
	for _, value := range values {
		_, _ = h.Write([]byte(value))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
