package toolchain_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/toolchain"
)

func TestVendoredTestlibMatchesPinnedProvenance(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(toolchain.TestlibPath)))
	if err != nil {
		t.Fatal(err)
	}
	if got := domain.SumBytes(data); got != domain.Digest(toolchain.TestlibHash) {
		t.Fatalf("vendored testlib digest = %q, want %q", got, toolchain.TestlibHash)
	}
	if !strings.Contains(string(data), `#define VERSION "`+toolchain.TestlibVersion+`"`) {
		t.Fatalf("vendored testlib does not declare VERSION %q", toolchain.TestlibVersion)
	}
}

func TestLoadLockAcceptsTheCompletePinnedContract(t *testing.T) {
	lock, err := toolchain.LoadLock(strings.NewReader(knownLockJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Validate(); err != nil {
		t.Fatal(err)
	}
	if lock.Builder.ImageID != digestA || len(lock.Toolchains) != 2 {
		t.Fatalf("unexpected lock: %#v", lock)
	}
	if _, err := lock.Digest(); err != nil {
		t.Fatalf("lock digest: %v", err)
	}
}

func TestLoadLockRejectsUnknownFields(t *testing.T) {
	withUnknown := strings.Replace(knownLockJSON, `"schema_version":`, `"future":true,"schema_version":`, 1)
	if _, err := toolchain.LoadLock(strings.NewReader(withUnknown)); err == nil {
		t.Fatal("unknown lock field was accepted")
	}
}

func TestLockRejectsMutableOrIncompleteToolchainMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*toolchain.Lock)
	}{
		{name: "floating builder tag", mutate: func(lock *toolchain.Lock) { lock.Builder.BaseRef = "golang:1.24" }},
		{name: "uppercase image digest", mutate: func(lock *toolchain.Lock) {
			lock.Builder.ImageID = domain.Digest("sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		}},
		{name: "duplicate toolchain IDs", mutate: func(lock *toolchain.Lock) { lock.Toolchains[1].ID = lock.Toolchains[0].ID }},
		{name: "testlib hash mismatch", mutate: func(lock *toolchain.Lock) { lock.Testlib.SHA256 = digestA }},
		{name: "protocol mismatch", mutate: func(lock *toolchain.Lock) { lock.ExecutionProtocol = "docker-direct-v1" }},
		{name: "missing static compiler flag", mutate: func(lock *toolchain.Lock) {
			lock.Toolchains[0].Command = []string{"/usr/bin/g++", "-std=c++20", "-O2"}
		}},
		{name: "split Go ldflags", mutate: func(lock *toolchain.Lock) {
			lock.Toolchains[1].Command = []string{"/usr/local/go/bin/go", "build", "-trimpath", "-ldflags=-s", "-w", "-buildid="}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lock, err := toolchain.LoadLock(strings.NewReader(knownLockJSON))
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&lock)
			if err := lock.Validate(); err == nil {
				t.Fatal("invalid toolchain lock was accepted")
			}
		})
	}
}

const (
	digestA domain.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB domain.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC domain.Digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

const knownLockJSON = `{
  "schema_version": "cpgen.toolchain-lock/v1",
  "execution_protocol": "docker-direct-v2",
  "builder": {
    "role": "builder",
    "image_id": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "base_ref": "golang:1.24.13-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac",
    "labels": {
      "org.cpgen.execution-protocol": "docker-direct-v2",
      "org.cpgen.image-role": "builder",
      "org.cpgen.image-schema": "cpgen.image/v1",
      "org.cpgen.testlib-sha256": "bb323e3c89285214966076e0d23d5a295c5f6126da7ff198c1276ddb95ecb1a0"
    }
  },
  "runtime": {
    "role": "runtime",
    "image_id": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "base_ref": "debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171",
    "labels": {
      "org.cpgen.execution-protocol": "docker-direct-v2",
      "org.cpgen.image-role": "runtime",
      "org.cpgen.image-schema": "cpgen.image/v1"
    }
  },
  "transfer": {
    "role": "transfer",
    "image_id": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    "base_ref": "debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171",
    "labels": {
      "org.cpgen.execution-protocol": "docker-direct-v2",
      "org.cpgen.image-role": "transfer",
      "org.cpgen.image-schema": "cpgen.image/v1"
    }
  },
  "toolchains": [
    {
      "id": "cpp20-gcc-bookworm-v1",
      "language": "CPP20",
      "image_id": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "command": ["/usr/bin/g++", "-std=c++20", "-O2", "-pipe", "-static", "-s", "-I/opt/cpgen/include", "/src/<entry>", "-o", "/result/files/main"],
      "environment": {"LANG":"C.UTF-8", "TZ":"UTC"},
      "output_path": "result/files/main"
    },
    {
      "id": "go124-bookworm-v1",
      "language": "GO",
      "image_id": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "command": ["/usr/local/go/bin/go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", "/result/files/main", "/src/<entry>"],
      "environment": {"CGO_ENABLED":"0", "GO111MODULE":"off", "GOPROXY":"off", "GOSUMDB":"off", "LANG":"C.UTF-8", "TZ":"UTC"},
      "output_path": "result/files/main"
    }
  ],
  "testlib": {
    "version": "0.9.45",
    "commit": "1e4e8a24c79c6bad3becbdb5a332ffc352b7d5dd",
    "sha256": "sha256:bb323e3c89285214966076e0d23d5a295c5f6126da7ff198c1276ddb95ecb1a0",
    "path": "third_party/testlib/testlib.h"
  }
}`
