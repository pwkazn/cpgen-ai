package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type sandboxArtifactCrashConfig struct {
	Database, BlobRoot, Boundary string
	Identity                     port.SandboxAuthorizationIdentity
	Declaration                  port.ArtifactDeclaration
	Now                          time.Time
}

func TestSandboxArtifactDeterministicPublicationRecoversUnsealedProcessCrash(t *testing.T) {
	for _, boundary := range []string{"before_dispatch", "partial_write"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			f := newCoordinatorFixture(t, "d9", domain.BudgetLimits{MaxArtifactBytes: 4096, MaxActiveTimeMilliseconds: 100000})
			root := filepath.Join(t.TempDir(), "blobs")
			identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000d9", LogicalOperationID: "deterministic-source", Kind: domain.CallSandboxCompile, ScopeDigest: domain.SumBytes([]byte("content")), ExpectedRunVersion: 2}
			declaration := port.ArtifactDeclaration{Role: domain.ArtifactSource, MediaType: "text/plain", LogicalPath: "source/main.cpp", MaxBytes: 4096, Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "solution"}}
			raw, err := json.Marshal(sandboxArtifactCrashConfig{f.path, root, boundary, identity, declaration, f.clock.Now()})
			if err != nil {
				t.Fatal(err)
			}
			childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			command := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestSandboxArtifactCrashHelper$")
			command.Env = append(os.Environ(), "CPGEN_SANDBOX_ARTIFACT_CRASH_FIXTURE="+string(raw))
			output, err := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 73 {
				t.Fatalf("crash helper: %v %s", err, output)
			}
			blobs, err := blob.NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			sink, err := application.NewSandboxArtifactSink(f.store, blobs, f.clock, identity)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sink.Prepare(ctx, declaration); err == nil {
				t.Fatal("stream writer implicitly replayed an unknown unsealed execution")
			}
			content := []byte("complete deterministic source\n")
			pending, err := sink.Publish(ctx, declaration, content)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := blobs.OpenVerified(ctx, pending.Blob)
			if err != nil {
				t.Fatal(err)
			}
			actual, readErr := io.ReadAll(reader)
			if err := errors.Join(readErr, reader.Close()); err != nil || string(actual) != string(content) {
				t.Fatalf("unsealed bytes leaked: %q %v", actual, err)
			}
			again, err := sink.Publish(ctx, declaration, content)
			if err != nil || again.WriterTokenID != pending.WriterTokenID || again.CallID != pending.CallID {
				t.Fatalf("writer identity changed: %v", err)
			}
			budget, err := f.store.BudgetSnapshot(ctx, f.runID)
			if err != nil || budget.Remaining[domain.BudgetArtifactPhysicalNewBytes] != 4096-int64(len(content)) {
				t.Fatalf("unsealed recovery charged twice: %+v %v", budget, err)
			}
		})
	}
}

type sandboxBeforeDispatchCrashStore struct{ *sqlite.Store }

func (s sandboxBeforeDispatchCrashStore) BeginDispatch(context.Context, domain.BeginDispatchRequest) (domain.DispatchGrant, error) {
	os.Exit(73)
	return domain.DispatchGrant{}, errors.New("unreachable")
}

func TestSandboxArtifactCrashHelper(t *testing.T) {
	raw := os.Getenv("CPGEN_SANDBOX_ARTIFACT_CRASH_FIXTURE")
	if raw == "" {
		t.Skip("subprocess helper")
	}
	var config sandboxArtifactCrashConfig
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	clock := newRecordingClock(config.Now)
	store, err := sqlite.OpenWithClock(ctx, sqlite.Config{Path: config.Database, BusyTimeout: time.Second, MaxReaders: 2}, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	blobs, err := blob.NewStore(config.BlobRoot)
	if err != nil {
		t.Fatal(err)
	}
	var ledger application.RunLLMStore = store
	if config.Boundary == "before_dispatch" {
		ledger = sandboxBeforeDispatchCrashStore{store}
	}
	sink, err := application.NewSandboxArtifactSink(ledger, blobs, clock, config.Identity)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := sink.Prepare(ctx, config.Declaration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("unpublished partial bytes")); err != nil {
		t.Fatal(err)
	}
	os.Exit(73)
}
