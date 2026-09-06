package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	watchdogprotocol "cpgen/internal/watchdog"
	"github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

type watchdogSession struct {
	client        *watchdogprotocol.Client
	controlRef    string
	controlDigest domain.Digest
	cleanup       func() error
	closeOnce     sync.Once
	closeErr      error
}

func (s *watchdogSession) ControlRecordRef() string         { return s.controlRef }
func (s *watchdogSession) ControlFileDigest() domain.Digest { return s.controlDigest }

func (s *watchdogSession) PreCreate(ctx context.Context, resource port.PlannedResource, labels map[string]string) error {
	return s.client.PreCreate(ctx, resource, labels)
}
func (s *watchdogSession) ResourceCreated(ctx context.Context, resource port.PlannedResource, id string) error {
	return s.client.ResourceCreated(ctx, resource, id)
}
func (s *watchdogSession) TargetPhase(ctx context.Context, resource port.PlannedResource, id string, remaining time.Duration) error {
	return s.client.TargetPhase(ctx, resource, id, remaining)
}
func (s *watchdogSession) Stopped(ctx context.Context, resource port.PlannedResource, id string) error {
	return s.client.Stopped(ctx, resource, id)
}
func (s *watchdogSession) Cleaned(ctx context.Context) error { return s.client.Cleaned(ctx) }
func (s *watchdogSession) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.client.Close()
		if s.cleanup != nil {
			s.closeErr = errors.Join(s.closeErr, s.cleanup())
		}
	})
	return s.closeErr
}

type dockerWatchdogReconciler struct {
	engine      Engine
	mu          sync.Mutex
	owned       map[string]*ownedContainer
	ownedVolume map[string]*ownedVolume
	eventCancel context.CancelFunc
	eventDone   chan struct{}
	eventErr    error
}

func newDockerWatchdogReconciler(engine Engine) (*dockerWatchdogReconciler, error) {
	if engine == nil {
		return nil, fmt.Errorf("watchdog Engine is required")
	}
	return &dockerWatchdogReconciler{engine: engine, owned: map[string]*ownedContainer{}, ownedVolume: map[string]*ownedVolume{}}, nil
}

func (r *dockerWatchdogReconciler) Begin(ctx context.Context, record watchdogprotocol.ControlRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eventCancel != nil {
		return fmt.Errorf("watchdog Engine events are already subscribed")
	}
	eventsCtx, cancel := context.WithCancel(ctx)
	filters := moby.Filters{}.Add("type", "container", "volume").Add("label", "org.cpgen.plan-digest="+string(record.Plan.PlanDigest))
	if record.RunID != "" {
		filters.Add("label", "org.cpgen.run="+string(record.RunID))
	}
	if record.AttemptID != "" {
		filters.Add("label", "org.cpgen.attempt="+string(record.AttemptID))
	}
	if record.SandboxExecutionID != "" {
		filters.Add("label", "org.cpgen.sandbox-execution="+string(record.SandboxExecutionID))
	}
	if record.LogicalOperationID != "" {
		filters.Add("label", "org.cpgen.logical-operation="+record.LogicalOperationID)
	}
	result := r.engine.Events(eventsCtx, moby.EventsListOptions{Filters: filters})
	r.eventCancel = cancel
	done := make(chan struct{})
	r.eventDone = done
	go func() {
		defer close(done)
		messages, failures := result.Messages, result.Err
		for messages != nil || failures != nil {
			select {
			case <-eventsCtx.Done():
				return
			case _, ok := <-messages:
				if !ok {
					messages = nil
				}
			case err, ok := <-failures:
				if !ok {
					failures = nil
					continue
				}
				if err != nil && !errors.Is(err, context.Canceled) {
					r.mu.Lock()
					r.eventErr = err
					r.mu.Unlock()
				}
			}
		}
		if eventsCtx.Err() == nil {
			r.mu.Lock()
			r.eventErr = fmt.Errorf("watchdog Engine event stream ended")
			r.mu.Unlock()
		}
	}()
	return nil
}

func (r *dockerWatchdogReconciler) Close() error {
	r.mu.Lock()
	cancel, done := r.eventCancel, r.eventDone
	r.eventCancel = nil
	r.eventDone = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}

func (r *dockerWatchdogReconciler) Observe(ctx context.Context, resource port.PlannedResource, expectedLabels map[string]string) (watchdogprotocol.Observation, error) {
	r.mu.Lock()
	eventErr := r.eventErr
	r.mu.Unlock()
	if eventErr != nil {
		return watchdogprotocol.Observation{}, eventErr
	}
	switch resource.Kind {
	case port.ResourceContainer:
		inspected, err := r.engine.ContainerInspect(ctx, resource.DeterministicName, moby.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			return watchdogprotocol.Observation{}, nil
		}
		if err != nil {
			return watchdogprotocol.Observation{}, err
		}
		actual := inspected.Container.Config
		foreign := actual == nil || len(expectedLabels) == 0 || !maps.Equal(actual.Labels, expectedLabels) || trimContainerName(inspected.Container.Name) != resource.DeterministicName
		observation := watchdogprotocol.Observation{Exists: true, ID: inspected.Container.ID, Kind: port.ResourceContainer, Foreign: foreign}
		if inspected.Container.State != nil {
			observation.Running = inspected.Container.State.Running
		}
		if !foreign {
			r.mu.Lock()
			r.owned[observation.ID] = &ownedContainer{
				resource: resource, name: resource.DeterministicName, id: observation.ID, labels: maps.Clone(expectedLabels), running: observation.Running,
			}
			r.mu.Unlock()
		}
		return observation, nil
	case port.ResourceVolume:
		inspected, err := r.engine.VolumeInspect(ctx, resource.DeterministicName, moby.VolumeInspectOptions{})
		if errdefs.IsNotFound(err) {
			return watchdogprotocol.Observation{}, nil
		}
		if err != nil {
			return watchdogprotocol.Observation{}, err
		}
		foreign := len(expectedLabels) == 0 || !maps.Equal(inspected.Volume.Labels, expectedLabels) || inspected.Volume.Name != resource.DeterministicName
		observation := watchdogprotocol.Observation{Exists: true, ID: inspected.Volume.Name, Kind: port.ResourceVolume, Foreign: foreign}
		if !foreign {
			r.mu.Lock()
			r.ownedVolume[observation.ID] = &ownedVolume{
				resource: resource, name: resource.DeterministicName, labels: maps.Clone(expectedLabels), driver: inspected.Volume.Driver,
				options: maps.Clone(inspected.Volume.Options),
			}
			r.mu.Unlock()
		}
		return observation, nil
	default:
		return watchdogprotocol.Observation{}, fmt.Errorf("watchdog does not support resource kind %q", resource.Kind)
	}
}

func (r *dockerWatchdogReconciler) Stop(ctx context.Context, observation watchdogprotocol.Observation) error {
	if !observation.Exists || observation.ID == "" || observation.Foreign {
		return fmt.Errorf("watchdog refused to stop an unowned observation")
	}
	r.mu.Lock()
	owned := r.owned[observation.ID]
	volume := r.ownedVolume[observation.ID]
	r.mu.Unlock()
	if observation.Kind == port.ResourceVolume || (observation.Kind == "" && volume != nil) {
		if volume == nil {
			return fmt.Errorf("watchdog has no exact volume ownership evidence for %q", observation.ID)
		}
		inspected, err := r.engine.VolumeInspect(ctx, observation.ID, moby.VolumeInspectOptions{})
		if errdefs.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if inspected.Volume.Name != volume.name || !maps.Equal(inspected.Volume.Labels, volume.labels) ||
			inspected.Volume.Driver != volume.driver || !maps.Equal(inspected.Volume.Options, volume.options) {
			return fmt.Errorf("watchdog volume ownership changed for %q", observation.ID)
		}
		_, err = r.engine.VolumeRemove(ctx, observation.ID, moby.VolumeRemoveOptions{Force: true})
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	if owned == nil {
		return fmt.Errorf("watchdog has no exact ownership evidence for %q", observation.ID)
	}
	_, err := portableStop(ctx, r.engine, observation.ID, func(result moby.ContainerInspectResult) error {
		return verifyContainerOwnership(result, owned)
	})
	return err
}

type InProcessWatchdogController struct {
	token     string
	tokenHash domain.Digest
	engine    Engine
	service   watchdogprotocol.Service
}

func NewInProcessWatchdogController(token string, engine Engine) (*InProcessWatchdogController, error) {
	if len(token) < 32 {
		return nil, fmt.Errorf("watchdog token must contain at least 32 bytes")
	}
	if engine == nil {
		return nil, fmt.Errorf("watchdog Engine is required")
	}
	return &InProcessWatchdogController{
		token: token, tokenHash: domain.SumBytes([]byte(token)), engine: engine,
		service: watchdogprotocol.Service{PollInterval: 10 * time.Millisecond, LateCreateWindow: 100 * time.Millisecond, CleanupTimeout: 10 * time.Second},
	}, nil
}

func (c *InProcessWatchdogController) TokenDigest() domain.Digest { return c.tokenHash }

func (c *InProcessWatchdogController) Arm(ctx context.Context, record watchdogprotocol.ControlRecord) (WatchdogSession, error) {
	if record.TokenDigest != c.tokenHash {
		return nil, fmt.Errorf("watchdog token digest mismatch")
	}
	envelope, err := watchdogprotocol.NewEnvelope(record, c.token, "in-process")
	if err != nil {
		return nil, err
	}
	controlDir, err := os.MkdirTemp("", "cpgen-watchdog-")
	if err != nil {
		return nil, err
	}
	cleanupControl := func() error {
		return cleanupWatchdogControl(controlDir, filepath.Join(controlDir, "control.json"))
	}
	if err := prepareWatchdogControlDirectory(controlDir); err != nil {
		_ = cleanupControl()
		return nil, err
	}
	encoded, err := jsonMarshalEnvelope(envelope)
	if err != nil {
		_ = cleanupControl()
		return nil, err
	}
	controlPath := filepath.Join(controlDir, "control.json")
	if err := secureWriteWatchdogControl(controlPath, encoded); err != nil {
		_ = cleanupControl()
		return nil, err
	}
	reconciler, err := newDockerWatchdogReconciler(c.engine)
	if err != nil {
		_ = cleanupControl()
		return nil, err
	}
	owner, service := net.Pipe()
	go func() { _ = c.service.Serve(context.Background(), service, envelope, reconciler) }()
	client, err := watchdogprotocol.NewClient(owner, c.token, envelope.RecordDigest)
	if err != nil {
		_ = owner.Close()
		_ = cleanupControl()
		return nil, err
	}
	return &watchdogSession{client: client, controlRef: controlPath, controlDigest: domain.SumBytes(encoded), cleanup: cleanupControl}, nil
}

type DetachedWatchdogOptions struct {
	Config           Config
	EngineIdentity   domain.Digest
	ControlDirectory string
	Executable       string
	ArmTimeout       time.Duration
	CommandFactory   func(executable, controlPath string) *exec.Cmd
}

type DetachedWatchdogController struct {
	options   DetachedWatchdogOptions
	token     string
	tokenHash domain.Digest
}

func NewDetachedWatchdogController(options DetachedWatchdogOptions) (*DetachedWatchdogController, error) {
	if _, err := options.Config.Validate(runtime.GOOS); err != nil {
		return nil, err
	}
	if err := options.EngineIdentity.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(options.ControlDirectory) || options.ArmTimeout <= 0 {
		return nil, fmt.Errorf("watchdog control directory must be absolute and Arm timeout positive")
	}
	if options.Executable == "" {
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		options.Executable = executable
	}
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(tokenBytes[:])
	return &DetachedWatchdogController{options: options, token: token, tokenHash: domain.SumBytes([]byte(token))}, nil
}

func (c *DetachedWatchdogController) TokenDigest() domain.Digest { return c.tokenHash }

type detachedWatchdogPreparation struct {
	controller  *DetachedWatchdogController
	listener    net.Listener
	address     string
	controlDir  string
	controlPath string
	envelope    watchdogprotocol.Envelope
	encoded     []byte
	command     *exec.Cmd
	mu          sync.Mutex
	started     bool
	aborted     bool
}

func (p *detachedWatchdogPreparation) ControlRecordRef() string {
	return p.controlPath
}

func (p *detachedWatchdogPreparation) ControlFileDigest() domain.Digest {
	return domain.SumBytes(p.encoded)
}

func (p *detachedWatchdogPreparation) Abort() error {
	p.mu.Lock()
	if p.aborted {
		p.mu.Unlock()
		return nil
	}
	p.aborted = true
	command := p.command
	listener := p.listener
	p.mu.Unlock()
	var failures []error
	if command != nil && p.started && command.Process != nil {
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			failures = append(failures, err)
		}
	}
	if listener != nil {
		if err := listener.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	failures = append(failures, cleanupWatchdogControl(p.controlDir, p.controlPath))
	return errors.Join(failures...)
}

func (c *DetachedWatchdogController) Prepare(ctx context.Context, record watchdogprotocol.ControlRecord) (PreparedWatchdog, error) {
	if record.TokenDigest != c.tokenHash || record.EngineEndpoint != c.options.Config.EngineEndpoint || record.EngineIdentityDigest != c.options.EngineIdentity {
		return nil, fmt.Errorf("watchdog control record does not match the configured controller")
	}
	nonce := stableControlNonce(record)
	listener, address, controlDir, err := prepareWatchdogControl(c.options.ControlDirectory, nonce)
	if err != nil {
		return nil, err
	}
	controlPath := filepath.Join(controlDir, "control.json")
	envelope, err := watchdogprotocol.NewEnvelope(record, c.token, address)
	if err != nil {
		_ = listener.Close()
		_ = cleanupWatchdogControl(controlDir, controlPath)
		return nil, err
	}
	envelope.EngineConfig = &watchdogprotocol.EngineConfig{
		APIVersion: c.options.Config.APIVersion, BuilderImage: domain.Digest(c.options.Config.BuilderImage),
		RuntimeImage: domain.Digest(c.options.Config.RuntimeImage), TransferImage: domain.Digest(c.options.Config.TransferImage),
	}
	encoded, err := jsonMarshalEnvelope(envelope)
	if err != nil {
		_ = listener.Close()
		_ = cleanupWatchdogControl(controlDir, controlPath)
		return nil, err
	}
	if err := secureWriteWatchdogControl(controlPath, encoded); err != nil {
		_ = listener.Close()
		_ = cleanupWatchdogControl(controlDir, controlPath)
		return nil, err
	}
	command := (*exec.Cmd)(nil)
	if c.options.CommandFactory != nil {
		command = c.options.CommandFactory(c.options.Executable, controlPath)
	} else {
		command = exec.Command(c.options.Executable, "sandbox-watchdog", "--control", controlPath)
	}
	if command == nil {
		_ = listener.Close()
		_ = cleanupWatchdogControl(controlDir, controlPath)
		return nil, fmt.Errorf("watchdog command factory returned nil")
	}
	detachWatchdogCommand(command)
	if err := ctx.Err(); err != nil {
		_ = listener.Close()
		_ = cleanupWatchdogControl(controlDir, controlPath)
		return nil, err
	}
	return &detachedWatchdogPreparation{controller: c, listener: listener, address: address, controlDir: controlDir, controlPath: controlPath, envelope: envelope, encoded: encoded, command: command}, nil
}

func (p *detachedWatchdogPreparation) Start(ctx context.Context) (WatchdogSession, error) {
	if ctx == nil {
		return nil, fmt.Errorf("watchdog start context is required")
	}
	p.mu.Lock()
	if p.aborted {
		p.mu.Unlock()
		return nil, fmt.Errorf("watchdog preparation was aborted")
	}
	if p.started {
		p.mu.Unlock()
		return nil, fmt.Errorf("watchdog preparation was already started")
	}
	p.started = true
	command := p.command
	listener := p.listener
	p.mu.Unlock()
	if err := command.Start(); err != nil {
		_ = p.Abort()
		return nil, err
	}
	acceptDone := make(chan struct {
		conn net.Conn
		err  error
	}, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		acceptDone <- struct {
			conn net.Conn
			err  error
		}{conn: conn, err: acceptErr}
	}()
	armCtx, cancel := context.WithTimeout(ctx, p.controller.options.ArmTimeout)
	defer cancel()
	var accepted net.Conn
	select {
	case result := <-acceptDone:
		if result.err != nil {
			_ = p.Abort()
			return nil, result.err
		}
		accepted = result.conn
	case <-armCtx.Done():
		_ = p.Abort()
		return nil, armCtx.Err()
	}
	_ = listener.Close()
	_ = command.Process.Release()
	client, err := watchdogprotocol.NewClient(accepted, p.controller.token, p.envelope.RecordDigest)
	if err != nil {
		_ = accepted.Close()
		_ = p.Abort()
		return nil, err
	}
	return &watchdogSession{client: client, controlRef: p.controlPath, controlDigest: p.ControlFileDigest(), cleanup: func() error { return cleanupWatchdogControl(p.controlDir, p.controlPath) }}, nil
}

func (c *DetachedWatchdogController) Arm(ctx context.Context, record watchdogprotocol.ControlRecord) (WatchdogSession, error) {
	prepared, err := c.Prepare(ctx, record)
	if err != nil {
		return nil, err
	}
	session, err := prepared.Start(ctx)
	if err != nil {
		_ = prepared.Abort()
		return nil, err
	}
	return session, nil
}

func RunWatchdogService(ctx context.Context, controlPath string) error {
	data, err := secureReadWatchdogControl(controlPath, 1<<20)
	if err != nil {
		return err
	}
	envelope, err := watchdogprotocol.ParseEnvelope(data)
	if err != nil {
		return err
	}
	if envelope.EngineConfig == nil {
		return fmt.Errorf("watchdog envelope omits Engine configuration")
	}
	config := Config{
		EngineEndpoint: envelope.Record.EngineEndpoint, APIVersion: envelope.EngineConfig.APIVersion,
		BuilderImage: string(envelope.EngineConfig.BuilderImage), RuntimeImage: string(envelope.EngineConfig.RuntimeImage), TransferImage: string(envelope.EngineConfig.TransferImage),
		ExecutionProtocol: ExecutionProtocolDockerDirectV2,
	}
	report, err := CheckStatic(ctx, config)
	if err != nil {
		return err
	}
	if report.EngineIdentityDigest != envelope.Record.EngineIdentityDigest {
		return fmt.Errorf("watchdog Engine identity mismatch")
	}
	engine, err := NewEngineClient(config)
	if err != nil {
		return err
	}
	defer engine.Close()
	reconciler, err := newDockerWatchdogReconciler(engine)
	if err != nil {
		return err
	}
	conn, err := dialWatchdogControl(ctx, envelope.ControlAddress)
	if err != nil {
		return err
	}
	service := watchdogprotocol.Service{PollInterval: 100 * time.Millisecond, LateCreateWindow: 2 * time.Second, CleanupTimeout: 30 * time.Second}
	return service.Serve(ctx, conn, envelope, reconciler)
}

func randomControlNonce() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func jsonMarshalEnvelope(envelope watchdogprotocol.Envelope) ([]byte, error) {
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode watchdog envelope: %w", err)
	}
	return encoded, nil
}

var _ WatchdogController = (*InProcessWatchdogController)(nil)
var _ WatchdogController = (*DetachedWatchdogController)(nil)
var _ watchdogprotocol.Reconciler = (*dockerWatchdogReconciler)(nil)
