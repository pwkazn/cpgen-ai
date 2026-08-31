package watchdog_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/watchdog"
)

func TestWatchdogControlRecordRejectsInvalidTokenDigestAndWidenedPlan(t *testing.T) {
	record, token := controlRecord(t, time.Now().Add(time.Minute))
	envelope, err := watchdog.NewEnvelope(record, token, "test-control")
	if err != nil {
		t.Fatal(err)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*watchdog.Envelope){
		"token":  func(item *watchdog.Envelope) { item.Token = "wrong-token" },
		"digest": func(item *watchdog.Envelope) { item.RecordDigest = domain.SumBytes([]byte("forged")) },
		"widened plan": func(item *watchdog.Envelope) {
			item.Record.Plan.Resources[0].DeterministicName += "-widened"
		},
		"engine mismatch": func(item *watchdog.Envelope) {
			item.Record.EngineIdentityDigest = domain.SumBytes([]byte("other-engine"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := envelope
			bad.Record = envelope.Record.Clone()
			mutate(&bad)
			if err := bad.Validate(); err == nil {
				t.Fatalf("invalid envelope was accepted: %#v", bad)
			}
		})
	}
}

func TestWatchdogOwnerEOFStopsLateCreateAndForeignResourceIsNeverStopped(t *testing.T) {
	record, token := controlRecord(t, time.Now().Add(25*time.Millisecond))
	envelope, err := watchdog.NewEnvelope(record, token, "test-control")
	if err != nil {
		t.Fatal(err)
	}
	reconciler := newFakeReconciler()
	owner, service := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- (watchdog.Service{PollInterval: time.Millisecond, LateCreateWindow: 30 * time.Millisecond, CleanupTimeout: time.Second}).Serve(context.Background(), service, envelope, reconciler)
	}()
	client, err := watchdog.NewClient(owner, token, envelope.RecordDigest)
	if err != nil {
		t.Fatal(err)
	}
	resource := record.Plan.Resources[0]
	labels := exactLabels(record, resource)
	if err := client.PreCreate(context.Background(), resource, labels); err != nil {
		t.Fatal(err)
	}
	_ = owner.Close()
	go func() {
		time.Sleep(5 * time.Millisecond)
		reconciler.create(resource.DeterministicName, "late-id", labels, true)
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := reconciler.stoppedIDs(); !slices.Equal(got, []string{"late-id"}) {
		t.Fatalf("stopped IDs = %#v", got)
	}

	foreignRecord, foreignToken := controlRecord(t, time.Now().Add(time.Minute))
	foreignEnvelope, _ := watchdog.NewEnvelope(foreignRecord, foreignToken, "test-control-foreign")
	foreign := newFakeReconciler()
	foreign.create(foreignRecord.Plan.Resources[0].DeterministicName, "foreign-id", map[string]string{"foreign": "true"}, true)
	owner, service = net.Pipe()
	done = make(chan error, 1)
	go func() {
		done <- (watchdog.Service{PollInterval: time.Millisecond, LateCreateWindow: 10 * time.Millisecond, CleanupTimeout: time.Second}).Serve(context.Background(), service, foreignEnvelope, foreign)
	}()
	if _, err := watchdog.NewClient(owner, foreignToken, foreignEnvelope.RecordDigest); err == nil {
		t.Fatal("foreign baseline was acknowledged")
	}
	_ = owner.Close()
	if err := <-done; err == nil {
		t.Fatal("foreign baseline did not fail closed")
	}
	if len(foreign.stoppedIDs()) != 0 {
		t.Fatalf("foreign resource was stopped: %#v", foreign.stoppedIDs())
	}
}

func TestWatchdogDeadlineStopsAcknowledgedTarget(t *testing.T) {
	record, token := controlRecord(t, time.Now().Add(20*time.Millisecond))
	envelope, _ := watchdog.NewEnvelope(record, token, "deadline-control")
	reconciler := newFakeReconciler()
	owner, service := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- (watchdog.Service{PollInterval: time.Millisecond, LateCreateWindow: 5 * time.Millisecond, CleanupTimeout: time.Second}).Serve(context.Background(), service, envelope, reconciler)
	}()
	client, err := watchdog.NewClient(owner, token, envelope.RecordDigest)
	if err != nil {
		t.Fatal(err)
	}
	resource := record.Plan.Resources[0]
	labels := exactLabels(record, resource)
	if err := client.PreCreate(context.Background(), resource, labels); err != nil {
		t.Fatal(err)
	}
	reconciler.create(resource.DeterministicName, "target-id", labels, true)
	if err := client.ResourceCreated(context.Background(), resource, "target-id"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reconciler.stoppedIDs(), []string{"target-id"}) {
		t.Fatalf("deadline did not stop target: %#v", reconciler.stoppedIDs())
	}
	_ = owner.Close()
}

func TestWatchdogRejectsMismatchedOwnershipLabels(t *testing.T) {
	record, token := controlRecord(t, time.Now().Add(time.Minute))
	envelope, _ := watchdog.NewEnvelope(record, token, "labels-control")
	reconciler := newFakeReconciler()
	owner, service := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- (watchdog.Service{PollInterval: time.Millisecond, LateCreateWindow: 5 * time.Millisecond, CleanupTimeout: time.Second}).Serve(context.Background(), service, envelope, reconciler)
	}()
	client, err := watchdog.NewClient(owner, token, envelope.RecordDigest)
	if err != nil {
		t.Fatal(err)
	}
	resource := record.Plan.Resources[0]
	labels := exactLabels(record, resource)
	labels["org.cpgen.slice"] = "foreign"
	if err := client.PreCreate(context.Background(), resource, labels); err == nil {
		t.Fatal("mismatched ownership labels were acknowledged")
	}
	_ = owner.Close()
	if err := <-done; err == nil {
		t.Fatal("mismatched ownership labels did not fail closed")
	}
}

func TestWatchdogStopsCreateReturningAfterSafetyDeadline(t *testing.T) {
	record, token := controlRecord(t, time.Now().Add(15*time.Millisecond))
	envelope, _ := watchdog.NewEnvelope(record, token, "late-deadline-control")
	reconciler := newFakeReconciler()
	owner, service := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- (watchdog.Service{PollInterval: time.Millisecond, LateCreateWindow: 10 * time.Millisecond, CleanupTimeout: 40 * time.Millisecond}).Serve(context.Background(), service, envelope, reconciler)
	}()
	client, err := watchdog.NewClient(owner, token, envelope.RecordDigest)
	if err != nil {
		t.Fatal(err)
	}
	resource := record.Plan.Resources[0]
	labels := exactLabels(record, resource)
	if err := client.PreCreate(context.Background(), resource, labels); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	reconciler.create(resource.DeterministicName, "after-deadline-id", labels, true)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := reconciler.stoppedIDs(); !slices.Equal(got, []string{"after-deadline-id"}) {
		t.Fatalf("post-deadline Create was not stopped: %#v", got)
	}
	_ = owner.Close()
}

func controlRecord(t *testing.T, deadline time.Time) (watchdog.ControlRecord, string) {
	t.Helper()
	engine := domain.SumBytes([]byte("engine"))
	callOrdinal := 0
	resource := port.PlannedResource{
		Ordinal: 0, Kind: port.ResourceContainer, Role: port.ResourceTarget,
		DeterministicName: "cpgen-s0-0123456789abcdef0123456789abcdef-00-ctr-target",
		CreateCallOrdinal: &callOrdinal,
	}
	base := controlBaseLabels(engine, "compile-solution", resource)
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	resource.ExpectedLabelsDigest = domain.SumBytes(encoded)
	plan, err := port.NewContainerPlan(engine, []port.PlannedResource{resource}, 0)
	if err != nil {
		t.Fatal(err)
	}
	token := "watchdog-token-with-256-bits-of-test-entropy-000000000000000000000001"
	return watchdog.ControlRecord{
		SchemaVersion: watchdog.ControlRecordSchemaVersion, TokenDigest: domain.SumBytes([]byte(token)),
		EngineEndpoint: "npipe:////./pipe/docker_engine", EngineIdentityDigest: engine,
		LogicalOperationID: "compile-solution", Plan: plan, SafetyDeadlineUTC: deadline.UTC(),
	}, token
}

func exactLabels(record watchdog.ControlRecord, resource port.PlannedResource) map[string]string {
	labels := controlBaseLabels(record.EngineIdentityDigest, record.LogicalOperationID, resource)
	labels["org.cpgen.plan-digest"] = string(record.Plan.PlanDigest)
	labels["org.cpgen.call"] = "call"
	return labels
}

func controlBaseLabels(engine domain.Digest, logicalOperation string, resource port.PlannedResource) map[string]string {
	return map[string]string{
		"org.cpgen.attempt": "attempt", "org.cpgen.engine-digest": string(engine),
		"org.cpgen.execution-protocol": "docker-direct-v2", "org.cpgen.kind": string(resource.Kind),
		"org.cpgen.lease-epoch": "1", "org.cpgen.logical-operation": logicalOperation,
		"org.cpgen.name": resource.DeterministicName, "org.cpgen.ordinal": "0",
		"org.cpgen.role": string(resource.Role), "org.cpgen.run": "run", "org.cpgen.slice": "0",
	}
}

type fakeObserved struct {
	name    string
	id      string
	labels  map[string]string
	running bool
}

type fakeReconciler struct {
	mu        sync.Mutex
	resources map[string]*fakeObserved
	stopped   []string
}

func newFakeReconciler() *fakeReconciler {
	return &fakeReconciler{resources: map[string]*fakeObserved{}}
}

func (r *fakeReconciler) Begin(context.Context, watchdog.ControlRecord) error { return nil }
func (r *fakeReconciler) Close() error                                        { return nil }

func (r *fakeReconciler) Observe(_ context.Context, resource port.PlannedResource, labels map[string]string) (watchdog.Observation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.resources[resource.DeterministicName]
	if item == nil {
		return watchdog.Observation{}, nil
	}
	return watchdog.Observation{
		Exists: true, ID: item.id, Running: item.running,
		Foreign: len(labels) == 0 || !maps.Equal(labels, item.labels),
	}, nil
}

func (r *fakeReconciler) Stop(_ context.Context, observation watchdog.Observation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.resources {
		if item.id == observation.ID {
			item.running = false
			r.stopped = append(r.stopped, item.id)
			return nil
		}
	}
	return errors.New("resource disappeared")
}

func (r *fakeReconciler) create(name, id string, labels map[string]string, running bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources[name] = &fakeObserved{name: name, id: id, labels: maps.Clone(labels), running: running}
}

func (r *fakeReconciler) stoppedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.stopped)
}

var _ watchdog.Reconciler = (*fakeReconciler)(nil)
