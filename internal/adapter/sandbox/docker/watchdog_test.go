//go:build cpgen_slice0_probe

package docker

import (
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
	watchdogprotocol "cpgen/internal/watchdog"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	moby "github.com/moby/moby/client"
)

func TestWatchdogInProcessControllerStopsOwnerEOFAndRejectsForeignName(t *testing.T) {
	engine := newWatchdogEngine()
	controller, err := NewInProcessWatchdogController("in-process-watchdog-token-00000000000000000001", engine)
	if err != nil {
		t.Fatal(err)
	}
	record := watchdogRecord(t, controller.TokenDigest(), time.Now().Add(2*time.Second))
	session, err := controller.Arm(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	resource := record.Plan.Resources[0]
	labels := watchdogLabels(record, resource)
	if err := session.PreCreate(context.Background(), resource, labels); err != nil {
		t.Fatal(err)
	}
	engine.create(resource.DeterministicName, "target-id", labels, true)
	if err := session.ResourceCreated(context.Background(), resource, "target-id"); err != nil {
		t.Fatal(err)
	}
	if err := session.TargetPhase(context.Background(), resource, "target-id", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for engine.isRunning("target-id") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if engine.isRunning("target-id") || !engine.wasStopped("target-id") {
		t.Fatal("owner EOF did not stop the exact target")
	}

	foreignEngine := newWatchdogEngine()
	foreignEngine.create(resource.DeterministicName, "foreign-id", map[string]string{"foreign": "true"}, true)
	foreignController, _ := NewInProcessWatchdogController("foreign-watchdog-token-0000000000000000000001", foreignEngine)
	foreignRecord := watchdogRecord(t, foreignController.TokenDigest(), time.Now().Add(time.Minute))
	if _, err := foreignController.Arm(context.Background(), foreignRecord); err == nil {
		t.Fatal("same-name foreign resource was acknowledged")
	}
	if !foreignEngine.isRunning("foreign-id") || foreignEngine.wasStopped("foreign-id") {
		t.Fatal("foreign resource was stopped")
	}
}

func TestWatchdogEOFPreventsFurtherPreCreateAcknowledgement(t *testing.T) {
	engine := newWatchdogEngine()
	controller, err := NewInProcessWatchdogController("watchdog-eof-token-00000000000000000000000001", engine)
	if err != nil {
		t.Fatal(err)
	}
	record := watchdogRecord(t, controller.TokenDigest(), time.Now().Add(10*time.Millisecond))
	session, err := controller.Arm(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	resource := record.Plan.Resources[0]
	if err := session.PreCreate(context.Background(), resource, watchdogLabels(record, resource)); err == nil {
		t.Fatal("dead watchdog acknowledged a new Create")
	}
	_ = session.Close()
}

func TestWatchdogControlACLAndDetachedChildOwnerEOF(t *testing.T) {
	base := t.TempDir()
	listener, _, directory, err := prepareWatchdogControl(base, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	controlPath := filepath.Join(directory, "control.json")
	if err := secureWriteWatchdogControl(controlPath, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if data, err := secureReadWatchdogControl(controlPath, 64); err != nil || string(data) != "secret" {
		t.Fatalf("secure read = %q, %v", data, err)
	}
	if _, err := secureReadWatchdogControl("relative.json", 64); err == nil {
		t.Fatal("relative control path was accepted")
	}
	if err := cleanupWatchdogControl(directory, controlPath); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "child-finished")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest := domain.SumBytes([]byte("image"))
	endpoint := "unix:///var/run/docker.sock"
	if runtime.GOOS == "windows" {
		endpoint = "npipe:////./pipe/docker_engine"
	}
	controller, err := NewDetachedWatchdogController(DetachedWatchdogOptions{
		Config: Config{
			EngineEndpoint: endpoint, APIVersion: RequiredAPIVersion,
			BuilderImage: string(digest), RuntimeImage: string(digest), TransferImage: string(digest),
			ExecutionProtocol: ExecutionProtocolDockerDirectV2,
		},
		EngineIdentity: domain.SumBytes([]byte("engine")), ControlDirectory: t.TempDir(), Executable: executable, ArmTimeout: 5 * time.Second,
		CommandFactory: func(executable, controlPath string) *exec.Cmd {
			command := exec.Command(executable, "-test.run=^TestWatchdogDetachedChildHelper$")
			command.Env = append(os.Environ(), "CPGEN_WATCHDOG_CHILD=1", "CPGEN_WATCHDOG_CONTROL="+controlPath, "CPGEN_WATCHDOG_MARKER="+marker)
			return command
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record := watchdogRecord(t, controller.TokenDigest(), time.Now().Add(2*time.Second))
	record.EngineEndpoint = endpoint
	session, err := controller.Arm(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	resource := record.Plan.Resources[0]
	if err := session.PreCreate(context.Background(), resource, watchdogLabels(record, resource)); err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil {
			if string(data) != "ok" {
				t.Fatalf("detached child result = %q", data)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("detached watchdog child did not survive owner EOF to finish reconciliation")
}

func TestWatchdogDetachedChildHelper(t *testing.T) {
	if os.Getenv("CPGEN_WATCHDOG_CHILD") != "1" {
		return
	}
	controlPath := os.Getenv("CPGEN_WATCHDOG_CONTROL")
	marker := os.Getenv("CPGEN_WATCHDOG_MARKER")
	data, err := secureReadWatchdogControl(controlPath, 1<<20)
	if err == nil {
		var envelope watchdogprotocol.Envelope
		envelope, err = watchdogprotocol.ParseEnvelope(data)
		if err == nil {
			var conn net.Conn
			conn, err = dialWatchdogControl(context.Background(), envelope.ControlAddress)
			if err == nil {
				err = (watchdogprotocol.Service{PollInterval: time.Millisecond, LateCreateWindow: 20 * time.Millisecond, CleanupTimeout: time.Second}).Serve(context.Background(), conn, envelope, emptyWatchdogReconciler{})
			}
		}
	}
	value := []byte("ok")
	if err != nil {
		value = []byte("error: " + err.Error())
	}
	if writeErr := os.WriteFile(marker, value, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
}

func TestWatchdogDockerOwnerEOFCanary(t *testing.T) {
	executable := os.Getenv("CPGEN_WATCHDOG_EXECUTABLE")
	if executable == "" {
		t.Skip("set CPGEN_WATCHDOG_EXECUTABLE to a freshly built cpgen binary")
	}
	lockFile, err := os.Open("../../../../config/toolchains/docker-v1.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	lock, err := toolchain.LoadLock(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "unix:///var/run/docker.sock"
	if runtime.GOOS == "windows" {
		endpoint = "npipe:////./pipe/docker_engine"
	}
	config := Config{
		EngineEndpoint: endpoint, APIVersion: RequiredAPIVersion,
		BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID),
		ExecutionProtocol: ExecutionProtocolDockerDirectV2,
	}
	report, err := CheckStatic(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngineClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	controller, err := NewDetachedWatchdogController(DetachedWatchdogOptions{
		Config: config, EngineIdentity: report.EngineIdentityDigest, ControlDirectory: t.TempDir(), Executable: executable, ArmTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := randomControlNonce()
	if err != nil {
		t.Fatal(err)
	}
	callOrdinal := 0
	resource := port.PlannedResource{
		Ordinal: 0, Kind: port.ResourceContainer, Role: port.ResourceTarget,
		DeterministicName: "cpgen-s0-" + nonce + "-00-ctr-target", CreateCallOrdinal: &callOrdinal,
	}
	labels := maps.Clone(lock.Runtime.Labels)
	baseLabels := watchdogBaseLabels(report.EngineIdentityDigest, "watchdog-docker-canary", resource)
	for key, value := range baseLabels {
		labels[key] = value
	}
	resource.ExpectedLabelsDigest = digestLabels(baseLabels)
	plan, err := port.NewContainerPlan(report.EngineIdentityDigest, []port.PlannedResource{resource}, 0)
	if err != nil {
		t.Fatal(err)
	}
	record := watchdogprotocol.ControlRecord{
		SchemaVersion: watchdogprotocol.ControlRecordSchemaVersion, TokenDigest: controller.TokenDigest(),
		EngineEndpoint: endpoint, EngineIdentityDigest: report.EngineIdentityDigest,
		LogicalOperationID: "watchdog-docker-canary", Plan: plan, SafetyDeadlineUTC: time.Now().Add(20 * time.Second).UTC(),
	}
	for key, value := range map[string]string{
		"org.cpgen.plan-digest": string(plan.PlanDigest),
		"org.cpgen.call":        "call_00000000000000000000000000000091",
	} {
		labels[key] = value
	}
	session, err := controller.Arm(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.PreCreate(context.Background(), resource, labels); err != nil {
		t.Fatal(err)
	}
	pids := int64(8)
	oom := false
	created, err := engine.ContainerCreate(context.Background(), moby.ContainerCreateOptions{
		Name: resource.DeterministicName,
		Config: &container.Config{
			User: "65532:65532", Image: string(lock.Runtime.ImageID), Entrypoint: []string{"/bin/sleep", "30"},
			NetworkDisabled: true, Labels: labels, StopSignal: "SIGTERM",
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(network.NetworkNone), ReadonlyRootfs: true, CapDrop: []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"}, Runtime: "runc", Resources: container.Resources{
				Memory: 64 << 20, MemorySwap: 64 << 20, PidsLimit: &pids, OomKillDisable: &oom,
			},
		},
	})
	if err != nil {
		_ = session.Close()
		t.Fatal(err)
	}
	defer func() {
		zero := 0
		_, _ = engine.ContainerStop(context.Background(), created.ID, moby.ContainerStopOptions{Timeout: &zero})
		_, _ = engine.ContainerRemove(context.Background(), created.ID, moby.ContainerRemoveOptions{Force: true})
	}()
	if err := session.ResourceCreated(context.Background(), resource, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := session.TargetPhase(context.Background(), resource, created.ID, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ContainerStart(context.Background(), created.ID, moby.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		inspected, inspectErr := engine.ContainerInspect(context.Background(), created.ID, moby.ContainerInspectOptions{})
		if inspectErr == nil && inspected.Container.State != nil && !inspected.Container.State.Running && inspected.Container.State.Pid == 0 {
			if !maps.Equal(inspected.Container.Config.Labels, labels) || !slices.Equal(inspected.Container.Config.Entrypoint, []string{"/bin/sleep", "30"}) {
				t.Fatal("watchdog stopped a container with drifted identity")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("detached watchdog did not stop the target after owner EOF")
}

type emptyWatchdogReconciler struct{}

func (emptyWatchdogReconciler) Begin(context.Context, watchdogprotocol.ControlRecord) error {
	return nil
}
func (emptyWatchdogReconciler) Close() error { return nil }
func (emptyWatchdogReconciler) Observe(context.Context, port.PlannedResource, map[string]string) (watchdogprotocol.Observation, error) {
	return watchdogprotocol.Observation{}, nil
}
func (emptyWatchdogReconciler) Stop(context.Context, watchdogprotocol.Observation) error {
	return errors.New("unexpected Stop")
}

func watchdogRecord(t *testing.T, token domain.Digest, deadline time.Time) watchdogprotocol.ControlRecord {
	t.Helper()
	engine := domain.SumBytes([]byte("engine"))
	call := 0
	resource := port.PlannedResource{
		Ordinal: 0, Kind: port.ResourceContainer, Role: port.ResourceTarget,
		DeterministicName: "cpgen-s0-0123456789abcdef0123456789abcdef-00-ctr-target", CreateCallOrdinal: &call,
	}
	resource.ExpectedLabelsDigest = digestLabels(watchdogBaseLabels(engine, "watchdog-test", resource))
	plan, err := port.NewContainerPlan(engine, []port.PlannedResource{resource}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return watchdogprotocol.ControlRecord{
		SchemaVersion: watchdogprotocol.ControlRecordSchemaVersion, TokenDigest: token,
		EngineEndpoint: "npipe:////./pipe/docker_engine", EngineIdentityDigest: engine,
		LogicalOperationID: "watchdog-test", Plan: plan, SafetyDeadlineUTC: deadline.UTC(),
	}
}

func watchdogLabels(record watchdogprotocol.ControlRecord, resource port.PlannedResource) map[string]string {
	labels := watchdogBaseLabels(record.EngineIdentityDigest, record.LogicalOperationID, resource)
	labels["org.cpgen.plan-digest"] = string(record.Plan.PlanDigest)
	labels["org.cpgen.call"] = "call_00000000000000000000000000000001"
	return labels
}

func watchdogBaseLabels(engine domain.Digest, logicalOperation string, resource port.PlannedResource) map[string]string {
	return map[string]string{
		"org.cpgen.attempt": "attempt", "org.cpgen.engine-digest": string(engine),
		"org.cpgen.execution-protocol": ExecutionProtocolDockerDirectV2, "org.cpgen.kind": string(resource.Kind),
		"org.cpgen.lease-epoch": "1", "org.cpgen.logical-operation": logicalOperation,
		"org.cpgen.name": resource.DeterministicName, "org.cpgen.ordinal": "0",
		"org.cpgen.role": string(resource.Role), "org.cpgen.run": "run", "org.cpgen.slice": "0",
	}
}

type watchdogContainer struct {
	id      string
	name    string
	labels  map[string]string
	running bool
}

type watchdogEngine struct {
	Engine
	mu         sync.Mutex
	containers map[string]*watchdogContainer
	byName     map[string]string
	stopped    map[string]bool
}

func newWatchdogEngine() *watchdogEngine {
	return &watchdogEngine{containers: map[string]*watchdogContainer{}, byName: map[string]string{}, stopped: map[string]bool{}}
}

func (e *watchdogEngine) create(name, id string, labels map[string]string, running bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.containers[id] = &watchdogContainer{id: id, name: name, labels: maps.Clone(labels), running: running}
	e.byName[name] = id
}

func (e *watchdogEngine) ContainerInspect(_ context.Context, idOrName string, _ moby.ContainerInspectOptions) (moby.ContainerInspectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := idOrName
	if named := e.byName[idOrName]; named != "" {
		id = named
	}
	item := e.containers[id]
	if item == nil {
		return moby.ContainerInspectResult{}, errdefs.ErrNotFound
	}
	state := &container.State{Running: item.running}
	if item.running {
		state.Pid = 42
	}
	return moby.ContainerInspectResult{Container: container.InspectResponse{
		ID: item.id, Name: "/" + item.name, Config: &container.Config{Labels: maps.Clone(item.labels)}, State: state,
	}}, nil
}

func (e *watchdogEngine) ContainerStop(context.Context, string, moby.ContainerStopOptions) (moby.ContainerStopResult, error) {
	return moby.ContainerStopResult{}, nil
}
func (e *watchdogEngine) ContainerKill(_ context.Context, id string, _ moby.ContainerKillOptions) (moby.ContainerKillResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if item := e.containers[id]; item != nil {
		item.running = false
		e.stopped[id] = true
		return moby.ContainerKillResult{}, nil
	}
	return moby.ContainerKillResult{}, errdefs.ErrNotFound
}
func (e *watchdogEngine) ContainerWait(_ context.Context, id string, _ moby.ContainerWaitOptions) moby.ContainerWaitResult {
	result := make(chan container.WaitResponse, 1)
	failures := make(chan error, 1)
	e.mu.Lock()
	item := e.containers[id]
	if item != nil {
		item.running = false
		result <- container.WaitResponse{StatusCode: 0}
	} else {
		failures <- errdefs.ErrNotFound
	}
	e.mu.Unlock()
	close(result)
	close(failures)
	return moby.ContainerWaitResult{Result: result, Error: failures}
}
func (e *watchdogEngine) Events(ctx context.Context, _ moby.EventsListOptions) moby.EventsResult {
	messages := make(chan events.Message)
	failures := make(chan error)
	go func() {
		<-ctx.Done()
		close(messages)
		close(failures)
	}()
	return moby.EventsResult{Messages: messages, Err: failures}
}
func (e *watchdogEngine) isRunning(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.containers[id] != nil && e.containers[id].running
}
func (e *watchdogEngine) wasStopped(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopped[id]
}

var _ Engine = (*watchdogEngine)(nil)
var _ watchdogprotocol.Reconciler = emptyWatchdogReconciler{}
