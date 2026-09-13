package docker

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"cpgen/internal/domain"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/system"
	moby "github.com/moby/moby/client"
)

func TestDoctorChecksOnlyStaticEngineFactsInOrder(t *testing.T) {
	config := validConfig()
	config.BuilderImage = string(domain.SumBytes([]byte("builder")))
	config.RuntimeImage = string(domain.SumBytes([]byte("runtime")))
	config.TransferImage = string(domain.SumBytes([]byte("transfer")))
	engine := &recordingEngine{images: map[string]string{
		config.BuilderImage:  config.BuilderImage,
		config.RuntimeImage:  config.RuntimeImage,
		config.TransferImage: config.TransferImage,
	}}
	doctor, err := NewDoctor(engine, config, "windows")
	if err != nil {
		t.Fatal(err)
	}

	report, err := doctor.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"Ping", "ServerVersion", "Info",
		"ImageInspect:" + config.BuilderImage,
		"ImageInspect:" + config.RuntimeImage,
		"ImageInspect:" + config.TransferImage,
	}
	if !reflect.DeepEqual(engine.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", engine.calls, wantCalls)
	}
	if report.SchemaVersion != "cpgen.docker-static-report/v1" || report.DaemonID != "daemon-id" {
		t.Fatalf("unexpected report: %#v", report)
	}
	if err := report.EngineIdentityDigest.Validate(); err != nil {
		t.Fatalf("invalid Engine identity digest: %v", err)
	}
	if report.BuilderImageID != config.BuilderImage || report.RuntimeImageID != config.RuntimeImage || report.TransferImageID != config.TransferImage {
		t.Fatalf("report did not retain inspected image identities: %#v", report)
	}
}

func TestDoctorClassifiesConnectionAndCompatibilityFailures(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*recordingEngine, *Config)
		wantCode  domain.PortFailureCode
		wantClass domain.FailureClass
	}{
		{
			name: "unreachable",
			mutate: func(engine *recordingEngine, _ *Config) {
				engine.pingErr = errors.New("dial refused")
			},
			wantCode: domain.FailureUnavailable, wantClass: domain.FailureBlocked,
		},
		{
			name: "Windows daemon",
			mutate: func(engine *recordingEngine, _ *Config) {
				engine.version.Os = "windows"
			},
			wantCode: domain.FailureCapabilityMissing, wantClass: domain.FailureIncompatible,
		},
		{
			name: "old API",
			mutate: func(engine *recordingEngine, _ *Config) {
				engine.version.APIVersion = "1.39"
			},
			wantCode: domain.FailureVersionMismatch, wantClass: domain.FailureIncompatible,
		},
		{
			name: "server below pinned API",
			mutate: func(engine *recordingEngine, _ *Config) {
				engine.version.APIVersion = "1.54"
			},
			wantCode: domain.FailureVersionMismatch, wantClass: domain.FailureIncompatible,
		},
		{
			name: "image mismatch",
			mutate: func(engine *recordingEngine, config *Config) {
				engine.images[config.RuntimeImage] = string(domain.SumBytes([]byte("different")))
			},
			wantCode: domain.FailureCapabilityMissing, wantClass: domain.FailureIncompatible,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			engine := newRecordingEngine(config)
			test.mutate(engine, &config)
			doctor, err := NewDoctor(engine, config, "windows")
			if err != nil {
				t.Fatal(err)
			}
			_, err = doctor.Check(context.Background())
			var checkErr *CheckError
			if !errors.As(err, &checkErr) {
				t.Fatalf("error = %T %v, want CheckError", err, err)
			}
			if checkErr.Failure.Code != test.wantCode || checkErr.Failure.Class != test.wantClass {
				t.Fatalf("failure = %#v, want code %q class %q", checkErr.Failure, test.wantCode, test.wantClass)
			}
		})
	}
}

type recordingEngine struct {
	calls   []string
	pingErr error
	ping    moby.PingResult
	version moby.ServerVersionResult
	info    moby.SystemInfoResult
	images  map[string]string
}

func newRecordingEngine(config Config) *recordingEngine {
	return &recordingEngine{
		ping:    moby.PingResult{APIVersion: "1.55", OSType: "linux"},
		version: moby.ServerVersionResult{Version: "29.7.2", APIVersion: "1.55", MinAPIVersion: "1.24", Os: "linux", Arch: "amd64"},
		info: moby.SystemInfoResult{Info: system.Info{
			ID: "daemon-id", OSType: "linux", Architecture: "x86_64", CgroupVersion: "2", CgroupDriver: "cgroupfs",
			SecurityOptions: []string{"name=seccomp,profile=builtin", "name=cgroupns"},
		}},
		images: map[string]string{
			config.BuilderImage:  config.BuilderImage,
			config.RuntimeImage:  config.RuntimeImage,
			config.TransferImage: config.TransferImage,
		},
	}
}

func (e *recordingEngine) Ping(context.Context, moby.PingOptions) (moby.PingResult, error) {
	e.calls = append(e.calls, "Ping")
	if e.pingErr != nil {
		return moby.PingResult{}, e.pingErr
	}
	if e.ping.APIVersion == "" {
		e.ping = moby.PingResult{APIVersion: "1.55", OSType: "linux"}
	}
	return e.ping, nil
}

func (e *recordingEngine) ServerVersion(context.Context, moby.ServerVersionOptions) (moby.ServerVersionResult, error) {
	e.calls = append(e.calls, "ServerVersion")
	if e.version.APIVersion == "" {
		e.version = moby.ServerVersionResult{Version: "29.7.2", APIVersion: "1.55", MinAPIVersion: "1.24", Os: "linux", Arch: "amd64"}
	}
	return e.version, nil
}

func (e *recordingEngine) Info(context.Context, moby.InfoOptions) (moby.SystemInfoResult, error) {
	e.calls = append(e.calls, "Info")
	if e.info.Info.ID == "" {
		e.info = moby.SystemInfoResult{Info: system.Info{
			ID: "daemon-id", OSType: "linux", Architecture: "x86_64", CgroupVersion: "2", CgroupDriver: "cgroupfs",
			SecurityOptions: []string{"name=seccomp,profile=builtin", "name=cgroupns"},
		}}
	}
	return e.info, nil
}

func (e *recordingEngine) ImageInspect(_ context.Context, imageID string, _ ...moby.ImageInspectOption) (moby.ImageInspectResult, error) {
	e.calls = append(e.calls, "ImageInspect:"+imageID)
	got, ok := e.images[imageID]
	if !ok {
		return moby.ImageInspectResult{}, errors.New("image not found")
	}
	return moby.ImageInspectResult{InspectResponse: image.InspectResponse{ID: got}}, nil
}

func (e *recordingEngine) Close() error { return nil }
