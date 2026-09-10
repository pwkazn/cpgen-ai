package application_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestLLMValidationReceiptRetainsSafeRepairEvidenceAcrossRestart(t *testing.T) {
	f := newLLMReplayContentFixture(t, `{"schema_version":"cpgen.idea/v1","title":"private-response","secret-field":"private-content"}`)
	first, err := f.service.Generate(context.Background(), f.open, f.request)
	if err != nil || first.Failure == nil || first.Failure.Code != domain.FailureProtocol || len(first.Failure.Evidence) != 1 {
		t.Fatalf("validation result=%+v err=%v", first, err)
	}
	pending := first.Failure.Evidence[0]
	reader, err := f.blobs.OpenVerified(context.Background(), pending.Blob)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"secret-field", "private-response", "private-content", "private prompt", "fixture-key"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private data leaked: %s", private)
		}
	}
	service, err := application.NewReplayableLLMCalls(f.store, f.model, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		replayed, err := service.Generate(context.Background(), f.open, f.request)
		if err != nil || replayed.Failure == nil || !replayed.CallTrace.Equal(first.CallTrace) || f.httpCalls.Load() != 1 {
			t.Fatalf("replayed=%+v err=%v calls=%d", replayed, err, f.httpCalls.Load())
		}
		repair, err := service.ReadFormatRepair(context.Background(), f.open, f.request)
		if err != nil || repair == nil || len(repair.ErrorCodes) != 1 || repair.ErrorCodes[0] != port.StructuredOutputUnknownField || len(repair.FieldPaths) != 0 || len(repair.InvalidFragment) != 0 {
			t.Fatalf("repair=%+v err=%v", repair, err)
		}
	}
}

func TestLLMValidationReceiptRecoversBeforePhysicalAccounting(t *testing.T) {
	f := newLLMReplayContentFixture(t, `{"title":"private-response"}`)
	ledger := &interruptedLLMReplayLedger{Store: f.store, boundary: "complete"}
	service, err := application.NewReplayableLLMCalls(ledger, f.model, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Generate(context.Background(), f.open, f.request); err == nil {
		t.Fatal("expected accounting failure")
	}
	result, err := f.service.Generate(context.Background(), f.open, f.request)
	if err != nil || result.Failure == nil || len(result.Failure.Evidence) != 1 || f.httpCalls.Load() != 1 {
		t.Fatalf("recovery=%+v err=%v", result, err)
	}
	repair, err := f.service.ReadFormatRepair(context.Background(), f.open, f.request)
	if err != nil || repair == nil || repair.ErrorCodes[0] != port.StructuredOutputSchemaMissing {
		t.Fatalf("repair=%+v err=%v", repair, err)
	}
}

func TestLLMValidationReceiptIsNotInventedForHTTPFailures(t *testing.T) {
	f := newLLMReplayContentFixture(t, `invalid`, 401)
	result, err := f.service.Generate(context.Background(), f.open, f.request)
	if err != nil || result.Failure == nil || len(result.Failure.Evidence) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	repair, err := f.service.ReadFormatRepair(context.Background(), f.open, f.request)
	if err != nil || repair != nil || f.httpCalls.Load() != 1 {
		t.Fatalf("repair=%+v err=%v", repair, err)
	}
}
