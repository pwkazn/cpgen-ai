package watchdog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"reflect"
	"strconv"
	"sync"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type Observation struct {
	Exists  bool
	ID      string
	Running bool
	Foreign bool
}

type Reconciler interface {
	Begin(context.Context, ControlRecord) error
	Observe(context.Context, port.PlannedResource, map[string]string) (Observation, error)
	Stop(context.Context, Observation) error
	Close() error
}

type ForeignResourceError struct {
	Name string
	ID   string
}

func (e ForeignResourceError) Error() string {
	return fmt.Sprintf("same-name foreign resource %q (%s)", e.Name, e.ID)
}

type Service struct {
	PollInterval     time.Duration
	LateCreateWindow time.Duration
	CleanupTimeout   time.Duration
}

func (s Service) Validate() error {
	if s.PollInterval <= 0 || s.LateCreateWindow <= 0 || s.CleanupTimeout <= 0 {
		return fmt.Errorf("watchdog service durations must be positive")
	}
	return nil
}

type commandType string

const (
	commandHello           commandType = "HELLO"
	commandPreCreate       commandType = "PRECREATE"
	commandResourceCreated commandType = "RESOURCE_CREATED"
	commandTargetPhase     commandType = "TARGET_PHASE"
	commandStopped         commandType = "STOPPED"
	commandCleaned         commandType = "CLEANED"
)

type request struct {
	Sequence       uint64                `json:"sequence"`
	Type           commandType           `json:"type"`
	Token          string                `json:"token,omitempty"`
	RecordDigest   domain.Digest         `json:"record_digest,omitempty"`
	Resource       *port.PlannedResource `json:"resource,omitempty"`
	Labels         map[string]string     `json:"labels,omitempty"`
	ResourceID     string                `json:"resource_id,omitempty"`
	RemainingNanos int64                 `json:"remaining_nanos,omitempty"`
}

type response struct {
	Sequence uint64 `json:"sequence"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

type serviceState struct {
	record        ControlRecord
	labels        map[int]map[string]string
	created       map[int]string
	nextPreCreate int
	targetPhase   bool
	targetStopped bool
}

func (s Service) Serve(ctx context.Context, conn net.Conn, envelope Envelope, reconciler Reconciler) (returnErr error) {
	if err := s.Validate(); err != nil {
		return err
	}
	if ctx == nil || conn == nil || reconciler == nil {
		return fmt.Errorf("watchdog service dependencies are required")
	}
	defer conn.Close()
	if err := envelope.Validate(); err != nil {
		return err
	}
	if err := reconciler.Begin(ctx, envelope.Record); err != nil {
		return err
	}
	defer reconciler.Close()
	if err := baseline(ctx, envelope.Record, reconciler); err != nil {
		return err
	}
	decoder := json.NewDecoder(bufio.NewReader(conn))
	decoder.DisallowUnknownFields()
	encoder := json.NewEncoder(conn)
	requests := make(chan request)
	decodeErrors := make(chan error, 1)
	decodeStop := make(chan struct{})
	defer close(decodeStop)
	go func() {
		defer close(requests)
		for {
			var item request
			if err := decoder.Decode(&item); err != nil {
				select {
				case decodeErrors <- err:
				case <-decodeStop:
				}
				return
			}
			select {
			case requests <- item:
			case <-decodeStop:
				return
			}
		}
	}()

	deadlineDelay := time.Until(envelope.Record.SafetyDeadlineUTC)
	if deadlineDelay < 0 {
		deadlineDelay = 0
	}
	deadline := time.NewTimer(deadlineDelay)
	defer deadline.Stop()
	var first request
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-deadline.C:
		return s.reconcile(envelope.Record, nil, nil, reconciler, false)
	case item, ok := <-requests:
		if !ok {
			return <-decodeErrors
		}
		first = item
	}
	if first.Type != commandHello || first.Token != envelope.Token || first.RecordDigest != envelope.RecordDigest {
		_ = encoder.Encode(response{Sequence: first.Sequence, Error: "watchdog handshake rejected"})
		return fmt.Errorf("watchdog handshake rejected")
	}
	if err := encoder.Encode(response{Sequence: first.Sequence, OK: true}); err != nil {
		return err
	}
	state := &serviceState{record: envelope.Record.Clone(), labels: map[int]map[string]string{}, created: map[int]string{}}

	for {
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), s.reconcile(envelope.Record, state.labels, state.created, reconciler, state.hasUnknownCreate()))
		case <-deadline.C:
			return s.reconcile(envelope.Record, state.labels, state.created, reconciler, state.hasUnknownCreate())
		case item, open := <-requests:
			if !open {
				decodeErr := <-decodeErrors
				if errors.Is(decodeErr, io.EOF) {
					return s.reconcile(envelope.Record, state.labels, state.created, reconciler, state.hasUnknownCreate())
				}
				return errors.Join(decodeErr, s.reconcile(envelope.Record, state.labels, state.created, reconciler, state.hasUnknownCreate()))
			}
			cleaned, err := state.apply(ctx, item, reconciler)
			if err != nil {
				_ = encoder.Encode(response{Sequence: item.Sequence, Error: err.Error()})
				return errors.Join(err, s.reconcile(envelope.Record, state.labels, state.created, reconciler, state.hasUnknownCreate()))
			}
			if cleaned {
				if err := s.reconcile(envelope.Record, state.labels, state.created, reconciler, false); err != nil {
					_ = encoder.Encode(response{Sequence: item.Sequence, Error: err.Error()})
					return err
				}
			}
			if err := encoder.Encode(response{Sequence: item.Sequence, OK: true}); err != nil {
				return errors.Join(err, s.reconcile(envelope.Record, state.labels, state.created, reconciler, state.hasUnknownCreate()))
			}
			if cleaned {
				return nil
			}
		}
	}
}

func (s *serviceState) hasUnknownCreate() bool {
	for ordinal := range s.labels {
		if s.created[ordinal] == "" {
			return true
		}
	}
	return false
}

func baseline(ctx context.Context, record ControlRecord, reconciler Reconciler) error {
	for _, resource := range record.Plan.Resources {
		observed, err := reconciler.Observe(ctx, resource, nil)
		if err != nil {
			return err
		}
		if observed.Exists {
			return ForeignResourceError{Name: resource.DeterministicName, ID: observed.ID}
		}
	}
	return nil
}

func (s *serviceState) apply(ctx context.Context, item request, reconciler Reconciler) (bool, error) {
	if item.Type == commandCleaned {
		if item.Resource != nil || s.targetPhase && !s.targetStopped {
			return false, fmt.Errorf("CLEANED requires a stopped target and no resource payload")
		}
		return true, nil
	}
	if item.Resource == nil || item.Resource.Ordinal < 0 || item.Resource.Ordinal >= len(s.record.Plan.Resources) ||
		!reflect.DeepEqual(*item.Resource, s.record.Plan.Resources[item.Resource.Ordinal]) {
		return false, fmt.Errorf("command resource is not in the immutable plan")
	}
	resource := *item.Resource
	switch item.Type {
	case commandPreCreate:
		if resource.Ordinal != s.nextPreCreate || len(item.Labels) == 0 {
			return false, fmt.Errorf("PRECREATE is out of order or missing labels")
		}
		if err := validateControlLabels(s.record, resource, item.Labels); err != nil {
			return false, err
		}
		observed, err := reconciler.Observe(ctx, resource, nil)
		if err != nil {
			return false, err
		}
		if observed.Exists {
			return false, ForeignResourceError{Name: resource.DeterministicName, ID: observed.ID}
		}
		s.labels[resource.Ordinal] = maps.Clone(item.Labels)
		s.nextPreCreate++
		return false, nil
	case commandResourceCreated:
		labels, ok := s.labels[resource.Ordinal]
		if !ok || item.ResourceID == "" {
			return false, fmt.Errorf("resource ACK arrived before PRECREATE")
		}
		observed, err := reconciler.Observe(ctx, resource, labels)
		if err != nil {
			return false, err
		}
		if !observed.Exists || observed.Foreign || observed.ID != item.ResourceID {
			return false, fmt.Errorf("created resource identity was not observed exactly")
		}
		s.created[resource.Ordinal] = item.ResourceID
		return false, nil
	case commandTargetPhase:
		if resource.Role != port.ResourceTarget || s.created[resource.Ordinal] != item.ResourceID || item.RemainingNanos <= 0 || s.targetPhase {
			return false, fmt.Errorf("target phase ACK is invalid")
		}
		s.targetPhase = true
		return false, nil
	case commandStopped:
		if resource.Role != port.ResourceTarget || !s.targetPhase || s.created[resource.Ordinal] != item.ResourceID {
			return false, fmt.Errorf("target STOPPED ACK is invalid")
		}
		observed, err := reconciler.Observe(ctx, resource, s.labels[resource.Ordinal])
		if err != nil {
			return false, err
		}
		if observed.Foreign || observed.Running {
			return false, fmt.Errorf("target is not proven stopped")
		}
		s.targetStopped = true
		return false, nil
	default:
		return false, fmt.Errorf("unknown watchdog command %q", item.Type)
	}
}

func validateControlLabels(record ControlRecord, resource port.PlannedResource, labels map[string]string) error {
	want := map[string]string{
		"org.cpgen.name": resource.DeterministicName, "org.cpgen.role": string(resource.Role),
		"org.cpgen.ordinal": strconv.Itoa(resource.Ordinal), "org.cpgen.plan-digest": string(record.Plan.PlanDigest),
		"org.cpgen.engine-digest": string(record.EngineIdentityDigest),
	}
	for key, value := range want {
		if labels[key] != value {
			return fmt.Errorf("resource label %q does not match the control record", key)
		}
	}
	if labels["org.cpgen.call"] == "" {
		return fmt.Errorf("resource label %q does not match the control record", "org.cpgen.call")
	}
	if labels["org.cpgen.sandbox-execution"] == "" {
		return fmt.Errorf("resource label %q does not match the control record", "org.cpgen.sandbox-execution")
	}
	if record.RunID != "" {
		identityLabels := map[string]string{
			"org.cpgen.run":               string(record.RunID),
			"org.cpgen.attempt":           string(record.AttemptID),
			"org.cpgen.sandbox-execution": string(record.SandboxExecutionID),
			"org.cpgen.logical-operation": record.LogicalOperationID,
		}
		for key, value := range identityLabels {
			if labels[key] != value {
				return fmt.Errorf("resource label %q does not match the sealed execution identity", key)
			}
		}
		if resource.Kind == port.ResourceContainer {
			if err := domain.AttemptCallID(labels["org.cpgen.call"]).Validate(); err != nil {
				return fmt.Errorf("container call label is invalid: %w", err)
			}
		} else if labels["org.cpgen.call"] != "none" {
			return fmt.Errorf("non-container resource call label must be none")
		}
	}
	base := map[string]string{
		"org.cpgen.attempt":            labels["org.cpgen.attempt"],
		"org.cpgen.engine-digest":      labels["org.cpgen.engine-digest"],
		"org.cpgen.execution-protocol": labels["org.cpgen.execution-protocol"],
		"org.cpgen.kind":               labels["org.cpgen.kind"],
		"org.cpgen.logical-operation":  labels["org.cpgen.logical-operation"],
		"org.cpgen.name":               labels["org.cpgen.name"],
		"org.cpgen.ordinal":            labels["org.cpgen.ordinal"],
		"org.cpgen.role":               labels["org.cpgen.role"],
		"org.cpgen.run":                labels["org.cpgen.run"],
		"org.cpgen.slice":              labels["org.cpgen.slice"],
	}
	base["org.cpgen.sandbox-execution"] = labels["org.cpgen.sandbox-execution"]
	encoded, err := json.Marshal(base)
	if err != nil {
		return fmt.Errorf("encode resource ownership labels: %w", err)
	}
	if domain.SumBytes(encoded) != resource.ExpectedLabelsDigest {
		return fmt.Errorf("resource ownership label digest does not match the immutable plan")
	}
	return nil
}

func (s Service) reconcile(record ControlRecord, labels map[int]map[string]string, created map[int]string, reconciler Reconciler, holdUnknown bool) error {
	cleanupDeadline := time.Now().Add(s.CleanupTimeout)
	unknownHoldUntil := time.Time{}
	if holdUnknown {
		unknownHoldUntil = record.SafetyDeadlineUTC.Add(s.CleanupTimeout)
		heldDeadline := unknownHoldUntil.Add(2*s.LateCreateWindow + s.PollInterval)
		if heldDeadline.After(cleanupDeadline) {
			cleanupDeadline = heldDeadline
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), cleanupDeadline)
	defer cancel()
	quietSince := time.Now()
	ticker := time.NewTicker(s.PollInterval)
	defer ticker.Stop()
	for {
		activity := false
		for _, resource := range record.Plan.Resources {
			expected := labels[resource.Ordinal]
			observed, err := reconciler.Observe(ctx, resource, expected)
			if err != nil {
				return err
			}
			if observed.Foreign || observed.Exists && len(expected) == 0 {
				return ForeignResourceError{Name: resource.DeterministicName, ID: observed.ID}
			}
			if observed.Exists && observed.Running {
				activity = true
				if err := reconciler.Stop(ctx, observed); err != nil {
					return err
				}
			}
		}
		if activity {
			quietSince = time.Now()
		} else {
			effectiveQuietSince := quietSince
			if holdUnknown && unknownHoldUntil.After(effectiveQuietSince) {
				effectiveQuietSince = unknownHoldUntil
			}
			if time.Since(effectiveQuietSince) >= s.LateCreateWindow {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("watchdog reconciliation timed out: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

type Client struct {
	mu       sync.Mutex
	conn     net.Conn
	decoder  *json.Decoder
	encoder  *json.Encoder
	sequence uint64
}

func NewClient(conn net.Conn, token string, recordDigest domain.Digest) (*Client, error) {
	if conn == nil {
		return nil, fmt.Errorf("watchdog connection is required")
	}
	client := &Client{conn: conn, decoder: json.NewDecoder(bufio.NewReader(conn)), encoder: json.NewEncoder(conn)}
	if err := client.call(context.Background(), request{Type: commandHello, Token: token, RecordDigest: recordDigest}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

func (c *Client) PreCreate(ctx context.Context, resource port.PlannedResource, labels map[string]string) error {
	return c.call(ctx, request{Type: commandPreCreate, Resource: &resource, Labels: maps.Clone(labels)})
}

func (c *Client) ResourceCreated(ctx context.Context, resource port.PlannedResource, id string) error {
	return c.call(ctx, request{Type: commandResourceCreated, Resource: &resource, ResourceID: id})
}

func (c *Client) TargetPhase(ctx context.Context, resource port.PlannedResource, id string, remaining time.Duration) error {
	return c.call(ctx, request{Type: commandTargetPhase, Resource: &resource, ResourceID: id, RemainingNanos: int64(remaining)})
}

func (c *Client) Stopped(ctx context.Context, resource port.PlannedResource, id string) error {
	return c.call(ctx, request{Type: commandStopped, Resource: &resource, ResourceID: id})
}

func (c *Client) Cleaned(ctx context.Context) error {
	return c.call(ctx, request{Type: commandCleaned})
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) call(ctx context.Context, item request) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
		defer c.conn.SetDeadline(time.Time{})
	}
	c.sequence++
	item.Sequence = c.sequence
	if err := c.encoder.Encode(item); err != nil {
		return err
	}
	var reply response
	if err := c.decoder.Decode(&reply); err != nil {
		return err
	}
	if reply.Sequence != item.Sequence || !reply.OK {
		if reply.Error == "" {
			reply.Error = "invalid watchdog acknowledgement"
		}
		return fmt.Errorf("watchdog acknowledgement: %s", reply.Error)
	}
	return nil
}
