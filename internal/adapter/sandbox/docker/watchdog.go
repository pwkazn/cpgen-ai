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

type watchdogSession struct{ client *watchdogprotocol.Client }

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
func (s *watchdogSession) Close() error                      { return s.client.Close() }

type dockerWatchdogReconciler struct {
	engine      Engine
	mu          sync.Mutex
	owned       map[string]*ownedContainer
	eventCancel context.CancelFunc
	eventDone   chan struct{}
	eventErr    error
}

func newDockerWatchdogReconciler(engine Engine) (*dockerWatchdogReconciler, error) {
	if engine == nil {
		return nil, fmt.Errorf("watchdog Engine is required")
	}
	return &dockerWatchdogReconciler{engine: engine, owned: map[string]*ownedContainer{}}, nil
}

func (r *dockerWatchdogReconciler) Begin(ctx context.Context, record watchdogprotocol.ControlRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eventCancel != nil {
		return fmt.Errorf("watchdog Engine events are already subscribed")
	}
	eventsCtx, cancel := context.WithCancel(ctx)
	result := r.engine.Events(eventsCtx, moby.EventsListOptions{Filters: moby.Filters{}.
		Add("type", "container", "volume").Add("label", "org.cpgen.plan-digest="+string(record.Plan.PlanDigest))})
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
		observation := watchdogprotocol.Observation{Exists: true, ID: inspected.Container.ID, Foreign: foreign}
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
		foreign := len(expectedLabels) == 0 || !maps.Equal(inspected.Volume.Labels, expectedLabels)
		return watchdogprotocol.Observation{Exists: true, ID: inspected.Volume.Name, Foreign: foreign}, nil
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
	r.mu.Unlock()
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
	reconciler, err := newDockerWatchdogReconciler(c.engine)
	if err != nil {
		return nil, err
	}
	owner, service := net.Pipe()
	go func() { _ = c.service.Serve(context.Background(), service, envelope, reconciler) }()
	client, err := watchdogprotocol.NewClient(owner, c.token, envelope.RecordDigest)
	if err != nil {
		_ = owner.Close()
		return nil, err
	}
	return &watchdogSession{client: client}, nil
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

func (c *DetachedWatchdogController) Arm(ctx context.Context, record watchdogprotocol.ControlRecord) (WatchdogSession, error) {
	if record.TokenDigest != c.tokenHash || record.EngineEndpoint != c.options.Config.EngineEndpoint || record.EngineIdentityDigest != c.options.EngineIdentity {
		return nil, fmt.Errorf("watchdog control record does not match the configured controller")
	}
	nonce, err := randomControlNonce()
	if err != nil {
		return nil, err
	}
	listener, address, controlDir, err := prepareWatchdogControl(c.options.ControlDirectory, nonce)
	if err != nil {
		return nil, err
	}
	defer listener.Close()
	controlPath := filepath.Join(controlDir, "control.json")
	envelope, err := watchdogprotocol.NewEnvelope(record, c.token, address)
	if err != nil {
		_ = cleanupWatchdogControl(controlDir, controlPath)
		return nil, err
	}
	envelope.EngineConfig = &watchdogprotocol.EngineConfig{
		APIVersion: c.options.Config.APIVersion, BuilderImage: domain.Digest(c.options.Config.BuilderImage),
		RuntimeImage: domain.Digest(c.options.Config.RuntimeImage), TransferImage: domain.Digest(c.options.Config.TransferImage),
	}
	encoded, err := jsonMarshalEnvelope(envelope)
	if err != nil {
		_ = cleanupWatchdogControl(controlDir, controlPath)
		return nil, err
	}
	if err := secureWriteWatchdogControl(controlPath, encoded); err != nil {
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
		_ = cleanupWatchdogControl(controlDir, controlPath)
		return nil, fmt.Errorf("watchdog command factory returned nil")
	}
	detachWatchdogCommand(command)
	if err := command.Start(); err != nil {
		_ = cleanupWatchdogControl(controlDir, controlPath)
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
	armCtx, cancel := context.WithTimeout(ctx, c.options.ArmTimeout)
	defer cancel()
	var accepted net.Conn
	select {
	case result := <-acceptDone:
		if result.err != nil {
			_ = command.Process.Kill()
			_ = cleanupWatchdogControl(controlDir, controlPath)
			return nil, result.err
		}
		accepted = result.conn
	case <-armCtx.Done():
		_ = command.Process.Kill()
		_ = cleanupWatchdogControl(controlDir, controlPath)
		return nil, armCtx.Err()
	}
	_ = command.Process.Release()
	client, err := watchdogprotocol.NewClient(accepted, c.token, envelope.RecordDigest)
	if err != nil {
		_ = accepted.Close()
		_ = cleanupWatchdogControl(controlDir, controlPath)
		return nil, err
	}
	if err := cleanupWatchdogControl(controlDir, controlPath); err != nil {
		_ = client.Close()
		return nil, err
	}
	return &watchdogSession{client: client}, nil
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
