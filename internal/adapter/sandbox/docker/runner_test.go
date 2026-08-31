//go:build cpgen_slice0_probe

package docker_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/transfer"
	"cpgen/internal/watchdog"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/volume"
	moby "github.com/moby/moby/client"
)

func TestVerifiedImportGrantOrderKeeperAndExport(t *testing.T) {
	fixture := newRunnerFixture(t)
	result, err := fixture.runner.Compile(context.Background(), fixture.auth, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	if result.Program == nil || result.Program.Blob.Digest != domain.SumBytes([]byte("binary")) {
		t.Fatalf("program artifact = %#v", result.Program)
	}
	if result.Program.CallID != fixture.calls[3] {
		t.Fatalf("program CallID = %q, want export call %q", result.Program.CallID, fixture.calls[3])
	}
	if !slices.Equal(result.CallTrace.PhysicalAttemptCallIDs, fixture.calls) || result.CallTrace.ResultAttemptCallID == nil || *result.CallTrace.ResultAttemptCallID != fixture.calls[2] {
		t.Fatalf("call trace = %#v", result.CallTrace)
	}

	events := fixture.events.snapshot()
	firstEngine := firstEventWithPrefix(events, "volume-create:", "create:")
	if firstEngine < 0 || countPrefix(events[:firstEngine], "open:") != len(fixture.request.SourceBundle.Files) || countPrefix(events[:firstEngine], "prepare:") != 4 {
		t.Fatalf("preflight did not precede Engine resources: %#v", events)
	}
	for _, role := range []port.ContainerRole{port.ContainerImport, port.ContainerKeeper, port.ContainerTarget, port.ContainerExport} {
		claim := indexOf(events, "claim:"+string(role))
		create := indexOf(events, "create:"+string(role))
		if claim < 0 || create != claim+2 { // watchdog pre-create is the only event between grant and dispatch
			t.Fatalf("role %s grant/create order is wrong: %#v", role, events)
		}
	}
	if indexOf(events, "wait:TARGET") > indexOf(events, "create:EXPORT") || indexOf(events, "start:KEEPER") > indexOf(events, "start:TARGET") {
		t.Fatalf("keeper/target/export order is wrong: %#v", events)
	}
	if indexOf(events, "finalize:program/main") > indexOf(events, "volume-remove:OUTPUT") {
		t.Fatalf("artifact was not finalized before output cleanup: %#v", events)
	}
	if fixture.engine.resourceCount() != 0 {
		t.Fatalf("fake Engine retained resources: %#v", fixture.engine)
	}
	if got := fixture.claims.states(); !slices.Equal(got, []string{"DISPATCHING", "DISPATCHING", "DISPATCHING", "DISPATCHING"}) {
		t.Fatalf("claim states = %#v", got)
	}
}

func TestVerifiedImportRejectsSameSizeCorruptionBeforeAnyCreate(t *testing.T) {
	fixture := newRunnerFixture(t)
	fixture.blobs.corrupt = fixture.request.SourceBundle.Files[0].Blob.Digest
	if _, err := fixture.runner.Compile(context.Background(), fixture.auth, fixture.request); err == nil {
		t.Fatal("same-size corrupt blob was accepted")
	}
	events := fixture.events.snapshot()
	if firstEventWithPrefix(events, "volume-create:", "create:") >= 0 {
		t.Fatalf("Engine resource created after failed verification: %#v", events)
	}
	if got := fixture.claims.states(); !slices.Equal(got, []string{"ABORTED_NO_DISPATCH", "ABORTED_NO_DISPATCH", "ABORTED_NO_DISPATCH", "ABORTED_NO_DISPATCH"}) {
		t.Fatalf("claim states = %#v", got)
	}
}

func TestGrantFailureAbortsWritersAndNeverDeletesForeignCollision(t *testing.T) {
	fixture := newRunnerFixture(t)
	fixture.engine.failCreateRole = port.ContainerKeeper
	fixture.engine.createForeignOnFailure = true
	_, err := fixture.runner.Compile(context.Background(), fixture.auth, fixture.request)
	if err == nil {
		t.Fatal("keeper collision was accepted")
	}
	events := fixture.events.snapshot()
	if countPrefix(events, "remove:foreign-") != 0 || !fixture.engine.hasForeignCollision() {
		t.Fatalf("foreign collision was deleted: %#v", events)
	}
	if countPrefix(events, "abort:program/main") != 1 {
		t.Fatalf("prepared writer was not aborted exactly once: %#v", events)
	}
	if got := fixture.claims.states(); !slices.Equal(got, []string{"DISPATCHING", "DISPATCHING", "ABORTED_NO_DISPATCH", "ABORTED_NO_DISPATCH"}) {
		t.Fatalf("claim states = %#v", got)
	}
}

type runnerFixture struct {
	runner  *docker.Runner
	auth    port.SandboxDispatchAuthorization
	request port.CompileRequest
	calls   []domain.AttemptCallID
	claims  *recordingClaims
	blobs   *recordingBlobs
	engine  *recordingDockerEngine
	events  *eventLog
}

func newRunnerFixture(t *testing.T) runnerFixture {
	t.Helper()
	events := &eventLog{}
	request := compileRequest()
	lock := toolchainLock(t)
	identity := planIdentity()
	plan, err := docker.BuildCompilePlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	calls := []domain.AttemptCallID{
		"call_00000000000000000000000000000011",
		"call_00000000000000000000000000000012",
		"call_00000000000000000000000000000013",
		"call_00000000000000000000000000000014",
	}
	probeIdentity := port.ProbeAuthorizationIdentity{
		LogicalOperationID: identity.LogicalOperationID, RunID: identity.RunID, AttemptID: identity.AttemptID,
		OwnerID: "owner_00000000000000000000000000000003", LeaseEpoch: identity.LeaseEpoch,
		ScopeDigest: domain.SumBytes([]byte("scope")), PlanDigest: plan.PlanDigest,
	}
	claims := newRecordingClaims(events, probeIdentity, calls)
	auth, err := port.NewSlice0ProbeAuthorization(probeIdentity, plan, claims)
	if err != nil {
		t.Fatal(err)
	}
	blobs := &recordingBlobs{events: events, data: map[domain.Digest][]byte{}}
	for index := range request.SourceBundle.Files {
		source := &request.SourceBundle.Files[index]
		data := bytes.Repeat([]byte{byte(len(blobs.data) + 1)}, int(source.Blob.Size))
		source.Blob.Digest = domain.SumBytes(data)
		blobs.data[source.Blob.Digest] = data
	}
	request.SourceBundle.Digest, err = port.ComputeSourceBundleDigest(request.SourceBundle)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the authorization because the fixture bytes are authoritative.
	plan, err = docker.BuildCompilePlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	probeIdentity.PlanDigest = plan.PlanDigest
	claims = newRecordingClaims(events, probeIdentity, calls)
	auth, err = port.NewSlice0ProbeAuthorization(probeIdentity, plan, claims)
	if err != nil {
		t.Fatal(err)
	}
	engine := newRecordingDockerEngine(events)
	engine.exportFiles = []transfer.StreamFile{{Path: "main", Mode: transfer.FrameModeExecutable, Data: []byte("binary")}}
	sink := &recordingArtifactSink{events: events}
	runner, err := docker.NewRunner(docker.RunnerOptions{
		Engine: engine,
		Config: docker.Config{
			EngineEndpoint: "npipe:////./pipe/docker_engine", APIVersion: docker.RequiredAPIVersion,
			BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID),
			ExecutionProtocol: docker.ExecutionProtocolDockerDirectV2,
		},
		Lock: lock, EngineIdentityDigest: identity.EngineIdentityDigest,
		Blobs: blobs, Artifacts: sink, Watchdog: &recordingWatchdog{events: events},
		Limits: docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runnerFixture{runner: runner, auth: auth, request: request, calls: calls, claims: claims, blobs: blobs, engine: engine, events: events}
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}
func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

type recordingClaims struct {
	mu          sync.Mutex
	events      *eventLog
	identity    port.ProbeAuthorizationIdentity
	calls       []domain.AttemptCallID
	roles       []port.ContainerRole
	stateValues []string
	next        int
}

func newRecordingClaims(events *eventLog, identity port.ProbeAuthorizationIdentity, calls []domain.AttemptCallID) *recordingClaims {
	states := make([]string, len(calls))
	for index := range states {
		states[index] = "AUTHORIZED"
	}
	return &recordingClaims{events: events, identity: identity, calls: slices.Clone(calls), stateValues: states}
}
func (c *recordingClaims) ClaimEnginePing(context.Context, port.ProbeAuthorizationIdentity) (domain.AttemptCallID, error) {
	return "", errors.New("not an Engine-ping ledger")
}
func (c *recordingClaims) ClaimContainer(_ context.Context, identity port.ProbeAuthorizationIdentity, ordinal int, role port.ContainerRole) (domain.AttemptCallID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if identity != c.identity || ordinal != c.next || c.next >= len(c.calls) {
		return "", errors.New("claim identity or ordinal mismatch")
	}
	c.events.add("claim:" + string(role))
	c.roles = append(c.roles, role)
	c.stateValues[c.next] = "DISPATCHING"
	call := c.calls[c.next]
	c.next++
	return call, nil
}
func (c *recordingClaims) AbortRemaining(_ context.Context, identity port.ProbeAuthorizationIdentity) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if identity != c.identity {
		return errors.New("abort identity mismatch")
	}
	for index := c.next; index < len(c.stateValues); index++ {
		c.stateValues[index] = "ABORTED_NO_DISPATCH"
	}
	c.next = len(c.stateValues)
	c.events.add("abort-remaining")
	return nil
}
func (c *recordingClaims) states() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.stateValues)
}

type recordingBlobs struct {
	events  *eventLog
	data    map[domain.Digest][]byte
	corrupt domain.Digest
}

func (b *recordingBlobs) OpenVerified(_ context.Context, blob domain.BlobRef) (port.VerifiedReadCloser, error) {
	b.events.add("open:" + string(blob.Digest))
	data, ok := b.data[blob.Digest]
	if !ok {
		return nil, errors.New("blob missing")
	}
	copyData := slices.Clone(data)
	if blob.Digest == b.corrupt && len(copyData) != 0 {
		copyData[0] ^= 0xff
	}
	return &recordingVerifiedReader{Reader: bytes.NewReader(copyData), blob: blob}, nil
}

type recordingVerifiedReader struct {
	*bytes.Reader
	blob domain.BlobRef
}

func (r *recordingVerifiedReader) BlobRef() domain.BlobRef { return r.blob }
func (r *recordingVerifiedReader) Close() error            { return nil }

type recordingArtifactSink struct {
	mu        sync.Mutex
	events    *eventLog
	finalized map[domain.SafeRelPath][]byte
}

func (s *recordingArtifactSink) Prepare(_ context.Context, declaration port.ArtifactDeclaration) (port.ArtifactWriter, error) {
	s.events.add("prepare:" + string(declaration.LogicalPath))
	return &recordingArtifactWriter{events: s.events, sink: s, declaration: declaration}, nil
}
func (s *recordingArtifactSink) PinExisting(context.Context, domain.BlobRef, port.ArtifactDeclaration) (domain.PendingArtifact, error) {
	return domain.PendingArtifact{}, errors.New("unexpected PinExisting")
}

type recordingArtifactWriter struct {
	events      *eventLog
	sink        *recordingArtifactSink
	declaration port.ArtifactDeclaration
	buffer      bytes.Buffer
	terminal    bool
}

func (w *recordingArtifactWriter) Write(data []byte) (int, error) { return w.buffer.Write(data) }
func (w *recordingArtifactWriter) Finalize(context.Context) (domain.PendingArtifact, error) {
	w.terminal = true
	w.events.add("finalize:" + string(w.declaration.LogicalPath))
	blob := domain.BlobRef{Digest: domain.SumBytes(w.buffer.Bytes()), Size: int64(w.buffer.Len())}
	w.sink.mu.Lock()
	if w.sink.finalized == nil {
		w.sink.finalized = make(map[domain.SafeRelPath][]byte)
	}
	w.sink.finalized[w.declaration.LogicalPath] = slices.Clone(w.buffer.Bytes())
	w.sink.mu.Unlock()
	return domain.PendingArtifact{
		Blob: blob, MediaType: w.declaration.MediaType, Role: w.declaration.Role, LogicalPath: w.declaration.LogicalPath,
		CallID: "call_00000000000000000000000000000099", ReservationID: "reservation_00000000000000000000000000000001",
		WriterTokenID: "writer_00000000000000000000000000000001", PinID: "pin_00000000000000000000000000000001",
		PhysicalNewBytes: blob.Size, Provenance: w.declaration.Provenance,
	}, nil
}

func (s *recordingArtifactSink) finalizedBytes(path domain.SafeRelPath) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.finalized[path])
}
func (w *recordingArtifactWriter) Abort(context.Context) error {
	if w.terminal {
		return nil
	}
	w.terminal = true
	w.events.add("abort:" + string(w.declaration.LogicalPath))
	return nil
}

type recordingWatchdog struct{ events *eventLog }

func (w *recordingWatchdog) TokenDigest() domain.Digest {
	return domain.SumBytes([]byte("recording-watchdog-token"))
}
func (w *recordingWatchdog) Arm(_ context.Context, record watchdog.ControlRecord) (docker.WatchdogSession, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	if record.TokenDigest != w.TokenDigest() {
		return nil, errors.New("recording watchdog token mismatch")
	}
	w.events.add("watchdog-arm")
	return &recordingWatchdogSession{events: w.events}, nil
}

type recordingWatchdogSession struct{ events *eventLog }

func (w *recordingWatchdogSession) PreCreate(_ context.Context, resource port.PlannedResource, _ map[string]string) error {
	w.events.add("watchdog-before:" + string(resource.Role))
	return nil
}
func (w *recordingWatchdogSession) ResourceCreated(_ context.Context, resource port.PlannedResource, _ string) error {
	w.events.add("watchdog-created:" + string(resource.Role))
	return nil
}
func (w *recordingWatchdogSession) TargetPhase(_ context.Context, resource port.PlannedResource, _ string, _ time.Duration) error {
	w.events.add("watchdog-start:" + string(resource.Role))
	return nil
}
func (w *recordingWatchdogSession) Stopped(_ context.Context, resource port.PlannedResource, _ string) error {
	w.events.add("watchdog-stopped:" + string(resource.Role))
	return nil
}
func (w *recordingWatchdogSession) Cleaned(context.Context) error {
	w.events.add("watchdog-cleaned")
	return nil
}
func (w *recordingWatchdogSession) Close() error {
	w.events.add("watchdog-close")
	return nil
}

type fakeContainer struct {
	id      string
	options moby.ContainerCreateOptions
	running bool
	foreign bool
}
type recordingDockerEngine struct {
	docker.Engine
	mu                     sync.Mutex
	events                 *eventLog
	containers             map[string]*fakeContainer
	names                  map[string]string
	volumes                map[string]volume.Volume
	createdOptions         []moby.ContainerCreateOptions
	volumeCreateOptions    []moby.VolumeCreateOptions
	nextID                 int
	exportFiles            []transfer.StreamFile
	targetStdout           []byte
	targetStderr           []byte
	targetExitCode         int
	targetOOMKilled        bool
	targetOOMEvent         bool
	targetWaitDelay        time.Duration
	expectedStdin          []byte
	receivedStdin          []byte
	failCreateRole         port.ContainerRole
	createForeignOnFailure bool
	failStartRole          port.ContainerRole
}

func newRecordingDockerEngine(events *eventLog) *recordingDockerEngine {
	return &recordingDockerEngine{events: events, containers: map[string]*fakeContainer{}, names: map[string]string{}, volumes: map[string]volume.Volume{}}
}
func (e *recordingDockerEngine) Close() error { return nil }
func (e *recordingDockerEngine) ContainerCreate(_ context.Context, options moby.ContainerCreateOptions) (moby.ContainerCreateResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.createdOptions = append(e.createdOptions, options)
	role := port.ContainerRole(options.Config.Labels["org.cpgen.role"])
	e.events.add("create:" + string(role))
	if role == e.failCreateRole {
		if e.createForeignOnFailure {
			e.nextID++
			id := "foreign-" + string(role)
			foreignOptions := options
			config := *options.Config
			config.Labels = map[string]string{"foreign": "true"}
			foreignOptions.Config = &config
			e.containers[id] = &fakeContainer{id: id, options: foreignOptions, foreign: true}
			e.names[options.Name] = id
		}
		return moby.ContainerCreateResult{}, errdefs.ErrAlreadyExists.WithMessage("simulated name collision")
	}
	e.nextID++
	id := "container-" + string(rune('0'+e.nextID))
	e.containers[id] = &fakeContainer{id: id, options: options}
	e.names[options.Name] = id
	return moby.ContainerCreateResult{ID: id}, nil
}
func (e *recordingDockerEngine) ContainerAttach(_ context.Context, id string, _ moby.ContainerAttachOptions) (moby.ContainerAttachResult, error) {
	e.mu.Lock()
	candidate := e.containers[id]
	e.mu.Unlock()
	if candidate == nil {
		return moby.ContainerAttachResult{}, errdefs.ErrNotFound
	}
	role := port.ContainerRole(candidate.options.Config.Labels["org.cpgen.role"])
	clientConn, serverConn := net.Pipe()
	if role == port.ContainerExport {
		go func() {
			defer serverConn.Close()
			var payload bytes.Buffer
			_, _ = transfer.WriteStream(context.Background(), &payload, e.exportFiles)
			_ = writeMuxFrame(serverConn, 1, payload.Bytes())
		}()
	} else if role == port.ContainerTarget {
		go func() {
			defer serverConn.Close()
			if len(e.expectedStdin) != 0 {
				input := make([]byte, len(e.expectedStdin))
				if _, err := io.ReadFull(serverConn, input); err == nil {
					e.mu.Lock()
					e.receivedStdin = input
					e.mu.Unlock()
				}
			}
			if len(e.targetStdout) != 0 {
				_ = writeMuxFrame(serverConn, 1, e.targetStdout)
			}
			if len(e.targetStderr) != 0 {
				_ = writeMuxFrame(serverConn, 2, e.targetStderr)
			}
		}()
	} else {
		go func() { _, _ = io.Copy(io.Discard, serverConn); _ = serverConn.Close() }()
	}
	return moby.ContainerAttachResult{HijackedResponse: moby.NewHijackedResponse(clientConn, "application/vnd.docker.raw-stream")}, nil
}

func writeMuxFrame(writer io.Writer, stream byte, data []byte) error {
	var header [8]byte
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}
func (e *recordingDockerEngine) ContainerStart(_ context.Context, id string, _ moby.ContainerStartOptions) (moby.ContainerStartResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	candidate := e.containers[id]
	if candidate == nil {
		return moby.ContainerStartResult{}, errdefs.ErrNotFound
	}
	role := candidate.options.Config.Labels["org.cpgen.role"]
	e.events.add("start:" + role)
	if port.ContainerRole(role) == e.failStartRole {
		return moby.ContainerStartResult{}, errors.New("simulated Start failure")
	}
	candidate.running = true
	return moby.ContainerStartResult{}, nil
}
func (e *recordingDockerEngine) ContainerWait(ctx context.Context, id string, _ moby.ContainerWaitOptions) moby.ContainerWaitResult {
	result := make(chan container.WaitResponse, 1)
	errors := make(chan error, 1)
	e.mu.Lock()
	candidate := e.containers[id]
	if candidate == nil {
		errors <- errdefs.ErrNotFound
		e.mu.Unlock()
		close(result)
		close(errors)
		return moby.ContainerWaitResult{Result: result, Error: errors}
	}
	role := candidate.options.Config.Labels["org.cpgen.role"]
	e.events.add("wait:" + role)
	delay := time.Duration(0)
	status := int64(0)
	if role == string(port.ContainerTarget) {
		delay = e.targetWaitDelay
		status = int64(e.targetExitCode)
	}
	e.mu.Unlock()
	go func() {
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				errors <- ctx.Err()
				close(result)
				close(errors)
				return
			case <-timer.C:
			}
		}
		e.mu.Lock()
		if current := e.containers[id]; current != nil {
			current.running = false
		}
		e.mu.Unlock()
		result <- container.WaitResponse{StatusCode: status}
		close(result)
		close(errors)
	}()
	return moby.ContainerWaitResult{Result: result, Error: errors}
}
func (e *recordingDockerEngine) ContainerInspect(_ context.Context, idOrName string, _ moby.ContainerInspectOptions) (moby.ContainerInspectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := idOrName
	if byName, ok := e.names[idOrName]; ok {
		id = byName
	}
	candidate := e.containers[id]
	if candidate == nil {
		return moby.ContainerInspectResult{}, errdefs.ErrNotFound
	}
	result := inspectFrom(candidate.options)
	result.Container.ID = candidate.id
	exitCode := 0
	oomKilled := false
	if candidate.options.Config.Labels["org.cpgen.role"] == string(port.ContainerTarget) {
		exitCode = e.targetExitCode
		oomKilled = e.targetOOMKilled
	}
	result.Container.State = &container.State{Running: candidate.running, ExitCode: exitCode, OOMKilled: oomKilled}
	if candidate.running {
		result.Container.State.Pid = 42
		result.Container.State.StartedAt = "2026-08-31T00:00:00Z"
	}
	return result, nil
}
func (e *recordingDockerEngine) ContainerStop(_ context.Context, id string, _ moby.ContainerStopOptions) (moby.ContainerStopResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	candidate := e.containers[id]
	if candidate == nil {
		return moby.ContainerStopResult{}, errdefs.ErrNotFound
	}
	e.events.add("stop:" + candidate.options.Config.Labels["org.cpgen.role"])
	candidate.running = false
	return moby.ContainerStopResult{}, nil
}
func (e *recordingDockerEngine) ContainerKill(_ context.Context, id string, _ moby.ContainerKillOptions) (moby.ContainerKillResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	candidate := e.containers[id]
	if candidate == nil {
		return moby.ContainerKillResult{}, errdefs.ErrNotFound
	}
	e.events.add("kill:" + candidate.options.Config.Labels["org.cpgen.role"])
	candidate.running = false
	return moby.ContainerKillResult{}, nil
}

func (e *recordingDockerEngine) Events(ctx context.Context, options moby.EventsListOptions) moby.EventsResult {
	messages := make(chan events.Message, 1)
	failures := make(chan error)
	go func() {
		if e.targetOOMKilled || e.targetOOMEvent {
			containerIDs := options.Filters["container"]
			for id := range containerIDs {
				messages <- events.Message{Type: events.ContainerEventType, Action: events.ActionOOM, Actor: events.Actor{ID: id}}
				break
			}
		}
		<-ctx.Done()
		close(messages)
		close(failures)
	}()
	return moby.EventsResult{Messages: messages, Err: failures}
}
func (e *recordingDockerEngine) ContainerRemove(_ context.Context, id string, _ moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	candidate := e.containers[id]
	if candidate == nil {
		return moby.ContainerRemoveResult{}, errdefs.ErrNotFound
	}
	e.events.add("remove:" + id)
	delete(e.names, candidate.options.Name)
	delete(e.containers, id)
	return moby.ContainerRemoveResult{}, nil
}
func (e *recordingDockerEngine) VolumeCreate(_ context.Context, options moby.VolumeCreateOptions) (moby.VolumeCreateResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.volumeCreateOptions = append(e.volumeCreateOptions, options)
	role := options.Labels["org.cpgen.role"]
	e.events.add("volume-create:" + role)
	created := volume.Volume{Name: options.Name, Driver: options.Driver, Labels: maps.Clone(options.Labels), Options: maps.Clone(options.DriverOpts)}
	e.volumes[options.Name] = created
	return moby.VolumeCreateResult{Volume: created}, nil
}
func (e *recordingDockerEngine) VolumeInspect(_ context.Context, name string, _ moby.VolumeInspectOptions) (moby.VolumeInspectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	item, ok := e.volumes[name]
	if !ok {
		return moby.VolumeInspectResult{}, errdefs.ErrNotFound
	}
	return moby.VolumeInspectResult{Volume: item}, nil
}
func (e *recordingDockerEngine) VolumeRemove(_ context.Context, name string, _ moby.VolumeRemoveOptions) (moby.VolumeRemoveResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	item, ok := e.volumes[name]
	if !ok {
		return moby.VolumeRemoveResult{}, errdefs.ErrNotFound
	}
	e.events.add("volume-remove:" + item.Labels["org.cpgen.role"])
	delete(e.volumes, name)
	return moby.VolumeRemoveResult{}, nil
}
func (e *recordingDockerEngine) resourceCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.containers) + len(e.volumes)
}
func (e *recordingDockerEngine) hasForeignCollision() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, candidate := range e.containers {
		if candidate.foreign {
			return true
		}
	}
	return false
}

func (e *recordingDockerEngine) creationHistory() []moby.ContainerCreateOptions {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.createdOptions)
}

func (e *recordingDockerEngine) volumeCreationHistory() []moby.VolumeCreateOptions {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.volumeCreateOptions)
}

func (e *recordingDockerEngine) stdinBytes() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.receivedStdin)
}

func firstEventWithPrefix(events []string, prefixes ...string) int {
	for index, event := range events {
		for _, prefix := range prefixes {
			if strings.HasPrefix(event, prefix) {
				return index
			}
		}
	}
	return -1
}
func countPrefix(events []string, prefix string) int {
	count := 0
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			count++
		}
	}
	return count
}
func indexOf(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return -1
}

var _ port.ProbeClaimStore = (*recordingClaims)(nil)
var _ port.VerifiedBlobReader = (*recordingBlobs)(nil)
var _ port.MeteredArtifactSink = (*recordingArtifactSink)(nil)
var _ docker.Engine = (*recordingDockerEngine)(nil)
