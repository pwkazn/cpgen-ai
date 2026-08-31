package watchdog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const (
	ControlRecordSchemaVersion domain.SchemaVersion = "cpgen.watchdog-control/v1"
	EnvelopeSchemaVersion      domain.SchemaVersion = "cpgen.watchdog-envelope/v1"
)

type ControlRecord struct {
	SchemaVersion        domain.SchemaVersion `json:"schema_version"`
	TokenDigest          domain.Digest        `json:"token_digest"`
	EngineEndpoint       string               `json:"engine_endpoint"`
	EngineIdentityDigest domain.Digest        `json:"engine_identity_digest"`
	LogicalOperationID   string               `json:"logical_operation_id"`
	Plan                 port.ContainerPlan   `json:"plan"`
	SafetyDeadlineUTC    time.Time            `json:"safety_deadline_utc"`
}

func (r ControlRecord) Clone() ControlRecord {
	r.Plan = r.Plan.Clone()
	return r
}

func (r ControlRecord) Validate() error {
	if r.SchemaVersion != ControlRecordSchemaVersion {
		return fmt.Errorf("control record schema version must be %q", ControlRecordSchemaVersion)
	}
	if err := r.TokenDigest.Validate(); err != nil {
		return fmt.Errorf("token digest: %w", err)
	}
	if r.EngineEndpoint == "" || strings.TrimSpace(r.EngineEndpoint) != r.EngineEndpoint {
		return fmt.Errorf("explicit Engine endpoint is required")
	}
	if err := r.EngineIdentityDigest.Validate(); err != nil {
		return fmt.Errorf("Engine identity digest: %w", err)
	}
	if r.LogicalOperationID == "" || len(r.LogicalOperationID) > 256 {
		return fmt.Errorf("logical operation ID is invalid")
	}
	if err := r.Plan.Validate(); err != nil {
		return err
	}
	if r.Plan.EngineIdentityDigest != r.EngineIdentityDigest {
		return fmt.Errorf("plan and control record Engine identities differ")
	}
	if r.SafetyDeadlineUTC.IsZero() || r.SafetyDeadlineUTC.Location() != time.UTC {
		return fmt.Errorf("safety deadline must be a nonzero UTC instant")
	}
	return nil
}

type Envelope struct {
	SchemaVersion  domain.SchemaVersion `json:"schema_version"`
	Token          string               `json:"token"`
	Record         ControlRecord        `json:"record"`
	RecordDigest   domain.Digest        `json:"record_digest"`
	ControlAddress string               `json:"control_address"`
	EngineConfig   *EngineConfig        `json:"engine_config,omitempty"`
}

type EngineConfig struct {
	APIVersion    string        `json:"api_version"`
	BuilderImage  domain.Digest `json:"builder_image"`
	RuntimeImage  domain.Digest `json:"runtime_image"`
	TransferImage domain.Digest `json:"transfer_image"`
}

func (c EngineConfig) Validate() error {
	if c.APIVersion == "" {
		return fmt.Errorf("watchdog Engine API version is required")
	}
	for label, digest := range map[string]domain.Digest{"builder": c.BuilderImage, "runtime": c.RuntimeImage, "transfer": c.TransferImage} {
		if err := digest.Validate(); err != nil {
			return fmt.Errorf("%s image: %w", label, err)
		}
	}
	return nil
}

func NewEnvelope(record ControlRecord, token, controlAddress string) (Envelope, error) {
	if err := record.Validate(); err != nil {
		return Envelope{}, err
	}
	envelope := Envelope{
		SchemaVersion: EnvelopeSchemaVersion, Token: token, Record: record.Clone(),
		RecordDigest: digestRecord(record), ControlAddress: controlAddress,
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

func (e Envelope) Validate() error {
	if e.SchemaVersion != EnvelopeSchemaVersion {
		return fmt.Errorf("watchdog envelope schema version must be %q", EnvelopeSchemaVersion)
	}
	if err := e.Record.Validate(); err != nil {
		return err
	}
	if len(e.Token) < 32 || domain.SumBytes([]byte(e.Token)) != e.Record.TokenDigest {
		return fmt.Errorf("watchdog token does not match its digest")
	}
	if err := e.RecordDigest.Validate(); err != nil {
		return fmt.Errorf("record digest: %w", err)
	}
	if digestRecord(e.Record) != e.RecordDigest {
		return fmt.Errorf("control record digest mismatch")
	}
	if e.ControlAddress == "" || strings.TrimSpace(e.ControlAddress) != e.ControlAddress {
		return fmt.Errorf("control address is required")
	}
	if e.EngineConfig != nil {
		if err := e.EngineConfig.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func digestRecord(record ControlRecord) domain.Digest {
	encoded, err := json.Marshal(record)
	if err != nil {
		panic(err)
	}
	return domain.SumBytes(encoded)
}

func ParseEnvelope(data []byte) (Envelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode watchdog envelope: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != nil && !errors.Is(err, io.EOF) {
		return Envelope{}, fmt.Errorf("decode trailing watchdog envelope: %w", err)
	} else if err == nil {
		return Envelope{}, fmt.Errorf("watchdog envelope contains a trailing JSON value")
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}
