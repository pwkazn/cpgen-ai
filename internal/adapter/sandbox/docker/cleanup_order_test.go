package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/watchdog"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/volume"
	moby "github.com/moby/moby/client"
)

func TestWatchdogCleansMountedVolumesAfterOwnerEOF(t *testing.T) {
	for _, foreignImport := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign_import=%v", foreignImport), func(t *testing.T) {
			fixture := newCleanupOrderFixture(t, 5*time.Second)
			engine := newCleanupOrderEngine()
			reconciler, err := newDockerWatchdogReconciler(engine)
			if err != nil {
				t.Fatal(err)
			}
			owner, service := net.Pipe()
			defer owner.Close()
			done := make(chan error, 1)
			go func() {
				done <- (watchdog.Service{PollInterval: time.Millisecond, LateCreateWindow: time.Millisecond, CleanupTimeout: time.Second}).Serve(context.Background(), service, fixture.envelope, reconciler)
			}()
			client, err := watchdog.NewClient(owner, fixture.envelope.Token, fixture.envelope.RecordDigest)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for i, planned := range fixture.envelope.Record.Plan.Resources {
				labels := fixture.labels[i]
				if err := client.PreCreate(ctx, planned, labels); err != nil {
					t.Fatal(err)
				}
				engine.add(planned, labels)
				if err := client.ResourceCreated(ctx, planned, planned.DeterministicName); err != nil {
					t.Fatal(err)
				}
			}
			if foreignImport {
				engine.mu.Lock()
				engine.containers["import"].Config.Labels["org.cpgen.call"] = "foreign"
				engine.mu.Unlock()
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if foreignImport {
					var foreign watchdog.ForeignResourceError
					if !errors.As(err, &foreign) {
						t.Fatalf("cleanup error = %v, want foreign-resource rejection", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("watchdog did not finish cleanup")
			}
			engine.assertCleanup(t, foreignImport)
		})
	}
}

func TestStartupReconcilerCleansMountedVolumesAfterContainers(t *testing.T) {
	fixture := newCleanupOrderFixture(t, 5*time.Second)
	engine := newCleanupOrderEngine()
	for i, planned := range fixture.envelope.Record.Plan.Resources {
		engine.add(planned, fixture.labels[i])
	}
	store := &cleanupOrderStore{execution: fixture.execution, control: fixture.control, resources: fixture.resources}
	reconciler, err := NewSandboxReconciler(SandboxReconcilerOptions{Engine: engine, Store: store, EngineIdentityDigest: fixture.execution.EngineIdentityDigest, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconciler.ReconcileRun(context.Background(), fixture.execution.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Completed || report.Cleaned != len(fixture.resources) || !store.finished {
		t.Fatalf("cleanup report = %+v, finished = %v", report, store.finished)
	}
	engine.assertCleanup(t, false)
}

func TestWatchdogPreservesUnacknowledgedCreateForStartupRecovery(t *testing.T) {
	for _, phase := range []domain.SandboxResourcePhase{domain.SandboxResourceDispatching, domain.SandboxResourceSent} {
		for _, boundary := range []string{"input", "import", "target"} {
			t.Run(string(phase)+"/"+boundary, func(t *testing.T) {
				fixture := newCleanupOrderFixture(t, 100*time.Millisecond)
				engine := newCleanupOrderEngine()
				reconciler, err := newDockerWatchdogReconciler(engine)
				if err != nil {
					t.Fatal(err)
				}
				owner, service := net.Pipe()
				defer owner.Close()
				done := make(chan error, 1)
				go func() {
					done <- (watchdog.Service{PollInterval: time.Millisecond, LateCreateWindow: time.Millisecond, CleanupTimeout: time.Second}).Serve(context.Background(), service, fixture.envelope, reconciler)
				}()
				client, err := watchdog.NewClient(owner, fixture.envelope.Token, fixture.envelope.RecordDigest)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				createdThrough := 0
				for i, planned := range fixture.envelope.Record.Plan.Resources {
					if err := client.PreCreate(ctx, planned, fixture.labels[i]); err != nil {
						t.Fatal(err)
					}
					engine.add(planned, fixture.labels[i])
					createdThrough = i
					if planned.DeterministicName == boundary {
						// The owner died after Docker Create but before the
						// exact ID was persisted and RESOURCE_CREATED sent.
						fixture.resources[i].EngineResourceID = ""
						fixture.resources[i].Phase = phase
						break
					}
					if err := client.ResourceCreated(ctx, planned, planned.DeterministicName); err != nil {
						t.Fatal(err)
					}
				}
				for i := createdThrough + 1; i < len(fixture.resources); i++ {
					fixture.resources[i].Phase = domain.SandboxResourcePlanned
					fixture.resources[i].EngineResourceID = ""
				}
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("watchdog did not finish its late-create window")
				}
				if boundary == "input" {
					if _, err := engine.VolumeInspect(ctx, boundary, moby.VolumeInspectOptions{}); err != nil {
						t.Fatalf("unacknowledged volume identity lost: %v", err)
					}
				} else {
					observed, err := engine.ContainerInspect(ctx, boundary, moby.ContainerInspectOptions{})
					if err != nil || observed.Container.State.Running {
						t.Fatalf("unacknowledged container must remain discoverable and stopped: %+v, %v", observed, err)
					}
				}
				store := &cleanupOrderStore{execution: fixture.execution, control: fixture.control, resources: fixture.resources}
				startup, err := NewSandboxReconciler(SandboxReconcilerOptions{Engine: engine, Store: store, EngineIdentityDigest: fixture.execution.EngineIdentityDigest, CleanupTimeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				report, err := startup.ReconcileRun(ctx, fixture.execution.RunID)
				if err != nil || !report.Completed || !store.finished {
					t.Fatalf("startup could not recover the unacknowledged create: report=%+v err=%v", report, err)
				}
				engine.mu.Lock()
				defer engine.mu.Unlock()
				if len(engine.containers) != 0 || len(engine.volumes) != 0 || engine.inUse != 0 {
					t.Fatalf("startup left containers=%v volumes=%v; in-use removals=%d", engine.containers, engine.volumes, engine.inUse)
				}
			})
		}
	}
}

type cleanupOrderFixture struct {
	envelope  watchdog.Envelope
	execution domain.SandboxExecution
	control   domain.SandboxWatchdogControl
	resources []domain.SandboxResource
	labels    []map[string]string
}

func newCleanupOrderFixture(t *testing.T, safetyAfter time.Duration) cleanupOrderFixture {
	t.Helper()
	now := time.Now().UTC()
	identity := PlanIdentity{RunID: "run_00000000000000000000000000000001", AttemptID: "attempt_00000000000000000000000000000001", SandboxExecutionID: "sandbox_00000000000000000000000000000001", LogicalOperationID: "cleanup-order", OperationNonce: "00000000000000000000000000000001", EngineIdentityDigest: domain.SumBytes([]byte("engine"))}
	builder := newResourcePlan(identity)
	builder.addVolume(port.ResourceInput)
	builder.addVolume(port.ResourceOutput)
	builder.addContainer(port.ResourceImport)
	builder.addContainer(port.ResourceKeeper)
	builder.addContainer(port.ResourceTarget)
	builder.addContainer(port.ResourceExport)
	for i, name := range []string{"input", "output", "import", "keeper", "target", "export"} {
		builder.items[i].DeterministicName = name
		builder.items[i].ExpectedLabelsDigest = digestLabels(baseResourceLabels(identity, builder.items[i]))
	}
	plan, err := port.NewContainerPlan(identity.EngineIdentityDigest, builder.items, 0)
	if err != nil {
		t.Fatal(err)
	}
	token := "cleanup-order-token-00000000000000000000000000000001"
	execution := domain.SandboxExecution{ID: identity.SandboxExecutionID, RunID: identity.RunID, AttemptID: identity.AttemptID, StageName: "sandbox", LogicalOperationID: identity.LogicalOperationID, ScopeDigest: domain.SumBytes([]byte("scope")), PlanDigest: plan.PlanDigest, EngineIdentityDigest: identity.EngineIdentityDigest, WatchdogTokenDigest: domain.SumBytes([]byte(token)), State: domain.SandboxExecutionCleanupPending, LifecycleVersion: 2, CreatedAt: now, UpdatedAt: now, SafetyDeadlineUTC: now.Add(safetyAfter), CleanupDeadlineUTC: now.Add(10 * time.Second)}
	record := watchdog.ControlRecord{SchemaVersion: watchdog.ControlRecordSchemaVersion, TokenDigest: execution.WatchdogTokenDigest, EngineEndpoint: "unix:///var/run/docker.sock", EngineIdentityDigest: execution.EngineIdentityDigest, RunID: execution.RunID, AttemptID: execution.AttemptID, SandboxExecutionID: execution.ID, ScopeDigest: execution.ScopeDigest, LogicalOperationID: execution.LogicalOperationID, Plan: plan, SafetyDeadlineUTC: execution.SafetyDeadlineUTC, CleanupDeadlineUTC: execution.CleanupDeadlineUTC}
	envelope, err := watchdog.NewEnvelope(record, token, "test-control")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := prepareWatchdogControlDirectory(directory); err != nil {
		t.Fatal(err)
	}
	execution.WatchdogControlRef = filepath.Join(directory, "control.json")
	raw, err := jsonMarshalEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureWriteWatchdogControl(execution.WatchdogControlRef, raw); err != nil {
		t.Fatal(err)
	}
	fixture := cleanupOrderFixture{envelope: envelope, execution: execution, control: domain.SandboxWatchdogControl{ExecutionID: execution.ID, ControlID: execution.WatchdogControlRef, ProcessRecordRef: execution.WatchdogControlRef, TokenDigest: execution.WatchdogTokenDigest, ControlFileDigest: domain.SumBytes(raw)}}
	for i, planned := range plan.Resources {
		call := domain.AttemptCallID(fmt.Sprintf("call_%032x", i+1))
		labels, err := ResourceLabels(identity, plan, planned, &call)
		if err != nil {
			t.Fatal(err)
		}
		fixture.labels = append(fixture.labels, labels)
		fixture.resources = append(fixture.resources, domain.SandboxResource{ID: domain.SandboxResourceID(fmt.Sprintf("resource_%032x", i+1)), ExecutionID: execution.ID, PlanOrdinal: planned.Ordinal, Kind: string(planned.Kind), Role: string(planned.Role), DeterministicName: planned.DeterministicName, ExpectedLabelsDigest: planned.ExpectedLabelsDigest, LabelsDigest: digestLabels(labels), PhysicalCallID: &call, EngineResourceID: planned.DeterministicName, EngineIdentityDigest: execution.EngineIdentityDigest, Phase: domain.SandboxResourceCleanupPending, Version: 2, CreatedAt: now, UpdatedAt: now})
	}
	return fixture
}

// Docker rejects volume removal while either a running or exited container
// references it; Force does not bypass this reference check.
type cleanupOrderEngine struct {
	Engine
	mu         sync.Mutex
	containers map[string]container.InspectResponse
	mounts     map[string][]string
	volumes    map[string]volume.Volume
	removed    []string
	inUse      int
}

func newCleanupOrderEngine() *cleanupOrderEngine {
	return &cleanupOrderEngine{containers: map[string]container.InspectResponse{}, mounts: map[string][]string{}, volumes: map[string]volume.Volume{}}
}

func (e *cleanupOrderEngine) add(resource port.PlannedResource, labels map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	name := resource.DeterministicName
	if resource.Kind == port.ResourceVolume {
		e.volumes[name] = volume.Volume{Name: name, Driver: "local", Labels: maps.Clone(labels)}
		return
	}
	state := &container.State{Running: resource.Role == port.ResourceTarget || resource.Role == port.ResourceKeeper}
	if state.Running {
		state.Pid = 42
	}
	e.containers[name] = container.InspectResponse{ID: name, Name: "/" + name, Config: &container.Config{Labels: maps.Clone(labels)}, State: state}
	switch resource.Role {
	case port.ResourceImport:
		e.mounts[name] = []string{"input"}
	case port.ResourceTarget:
		e.mounts[name] = []string{"input", "output"}
	default:
		e.mounts[name] = []string{"output"}
	}
}

func (e *cleanupOrderEngine) ContainerInspect(_ context.Context, id string, _ moby.ContainerInspectOptions) (moby.ContainerInspectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	item, ok := e.containers[id]
	if !ok {
		return moby.ContainerInspectResult{}, errdefs.ErrNotFound
	}
	config, state := *item.Config, *item.State
	config.Labels = maps.Clone(config.Labels)
	item.Config, item.State = &config, &state
	return moby.ContainerInspectResult{Container: item}, nil
}

func (e *cleanupOrderEngine) ContainerStop(_ context.Context, id string, _ moby.ContainerStopOptions) (moby.ContainerStopResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if item, ok := e.containers[id]; ok {
		item.State.Running, item.State.Pid = false, 0
		return moby.ContainerStopResult{}, nil
	}
	return moby.ContainerStopResult{}, errdefs.ErrNotFound
}

func (e *cleanupOrderEngine) ContainerKill(ctx context.Context, id string, _ moby.ContainerKillOptions) (moby.ContainerKillResult, error) {
	_, err := e.ContainerStop(ctx, id, moby.ContainerStopOptions{})
	return moby.ContainerKillResult{}, err
}

func (e *cleanupOrderEngine) ContainerWait(context.Context, string, moby.ContainerWaitOptions) moby.ContainerWaitResult {
	results := make(chan container.WaitResponse, 1)
	results <- container.WaitResponse{}
	return moby.ContainerWaitResult{Result: results}
}

func (e *cleanupOrderEngine) ContainerRemove(_ context.Context, id string, _ moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.containers[id]; !ok {
		return moby.ContainerRemoveResult{}, errdefs.ErrNotFound
	}
	delete(e.containers, id)
	delete(e.mounts, id)
	e.removed = append(e.removed, id)
	return moby.ContainerRemoveResult{}, nil
}

func (e *cleanupOrderEngine) VolumeInspect(_ context.Context, name string, _ moby.VolumeInspectOptions) (moby.VolumeInspectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	item, ok := e.volumes[name]
	if !ok {
		return moby.VolumeInspectResult{}, errdefs.ErrNotFound
	}
	item.Labels = maps.Clone(item.Labels)
	return moby.VolumeInspectResult{Volume: item}, nil
}

func (e *cleanupOrderEngine) VolumeRemove(_ context.Context, name string, _ moby.VolumeRemoveOptions) (moby.VolumeRemoveResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, mounts := range e.mounts {
		if slices.Contains(mounts, name) {
			e.inUse++
			return moby.VolumeRemoveResult{}, fmt.Errorf("volume %s is in use: %w", name, errdefs.ErrConflict)
		}
	}
	delete(e.volumes, name)
	e.removed = append(e.removed, name)
	return moby.VolumeRemoveResult{}, nil
}

func (*cleanupOrderEngine) Events(ctx context.Context, _ moby.EventsListOptions) moby.EventsResult {
	messages := make(chan events.Message)
	go func() { <-ctx.Done(); close(messages) }()
	return moby.EventsResult{Messages: messages}
}

func (e *cleanupOrderEngine) assertCleanup(t *testing.T, foreignImport bool) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.removed) == 0 || e.removed[0] != "target" {
		t.Fatalf("target was not cleaned first: %v", e.removed)
	}
	if foreignImport {
		if !slices.Equal(e.removed, []string{"target", "export", "keeper", "output"}) || len(e.containers) != 1 || len(e.volumes) != 1 {
			t.Fatalf("foreign import was changed or owned targets survived: removed=%v, containers=%v, volumes=%v", e.removed, e.containers, e.volumes)
		}
		return
	}
	if len(e.containers) != 0 || len(e.volumes) != 0 || e.inUse != 0 {
		t.Fatalf("cleanup left containers=%v volumes=%v; in-use volume attempts=%d", e.containers, e.volumes, e.inUse)
	}
}

type cleanupOrderStore struct {
	SandboxReconcileStore
	execution domain.SandboxExecution
	control   domain.SandboxWatchdogControl
	resources []domain.SandboxResource
	finished  bool
}

func (s *cleanupOrderStore) UnfinishedSandboxExecutions(context.Context, domain.RunID) ([]domain.SandboxExecution, error) {
	return []domain.SandboxExecution{s.execution}, nil
}
func (s *cleanupOrderStore) SandboxResources(context.Context, domain.SandboxExecutionID) ([]domain.SandboxResource, error) {
	return slices.Clone(s.resources), nil
}
func (s *cleanupOrderStore) GetSandboxWatchdogControl(context.Context, domain.SandboxExecutionID) (domain.SandboxWatchdogControl, error) {
	return s.control, nil
}
func (s *cleanupOrderStore) RecordResourceInterrupted(_ context.Context, command domain.RecordResourceInterruptedCommand) (domain.SandboxResource, error) {
	return s.update(command.ResourceID, command.ExpectedVersion, domain.SandboxResourceInterrupted)
}
func (s *cleanupOrderStore) AdvanceResource(_ context.Context, command domain.AdvanceResourceRequest) (domain.SandboxResource, error) {
	resource, err := s.update(command.ResourceID, command.ExpectedVersion, command.Phase)
	if err != nil {
		return resource, err
	}
	for i := range s.resources {
		if s.resources[i].ID == command.ResourceID {
			s.resources[i].EngineResourceID = command.EngineResourceID
			return s.resources[i], nil
		}
	}
	return domain.SandboxResource{}, errors.New("resource not found")
}
func (s *cleanupOrderStore) update(id domain.SandboxResourceID, version int64, phase domain.SandboxResourcePhase) (domain.SandboxResource, error) {
	for i := range s.resources {
		if s.resources[i].ID == id && s.resources[i].Version == version {
			s.resources[i].Version++
			s.resources[i].Phase = phase
			return s.resources[i], nil
		}
	}
	return domain.SandboxResource{}, errors.New("resource version mismatch")
}
func (s *cleanupOrderStore) RecordResourceStopProof(_ context.Context, command domain.RecordResourceStopProofCommand) (domain.SandboxResource, error) {
	return s.update(command.ResourceID, command.ExpectedVersion, domain.SandboxResourceStopped)
}
func (s *cleanupOrderStore) RecordResourceCleaned(_ context.Context, command domain.RecordResourceCleanedCommand) (domain.SandboxResource, error) {
	return s.update(command.ResourceID, command.ExpectedVersion, domain.SandboxResourceCleaned)
}
func (s *cleanupOrderStore) FinishCleanup(context.Context, domain.FinishCleanupCommand) (domain.SandboxExecution, error) {
	for _, resource := range s.resources {
		if resource.Phase != domain.SandboxResourceCleaned && resource.Phase != domain.SandboxResourceInterrupted {
			return domain.SandboxExecution{}, errors.New("resource not cleaned")
		}
	}
	s.finished = true
	return s.execution, nil
}
