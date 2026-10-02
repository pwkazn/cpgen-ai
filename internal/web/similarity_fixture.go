package web

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"

	"cpgen/internal/config"
)

// FixtureSimilarityEndpoint is a sentinel endpoint used only with
// --fixture-similarity. The fixture transport routes it to an in-process TLS
// server and never sends similarity requests to this public address.
const FixtureSimilarityEndpoint = "https://93.184.216.34/search"

const fixtureSimilarityKeyEnv = "CPGEN_SIMILARITY_FIXTURE_KEY"

type localSimilarityFixture struct {
	client    *http.Client
	server    *httptest.Server
	transport *http.Transport
	once      sync.Once
	err       error
	oldKey    string
	hadKey    bool
}

// NewWithCapacityAndSimilarityFixture enables the deterministic TLS
// similarity fixture for this local workbench process. The fixture returns a
// fixed low-score hit; it exercises the similarity protocol but does not
// assess originality.
func NewWithCapacityAndSimilarityFixture(ctx context.Context, cfg config.Config, capacity int) (*Server, error) {
	if cfg.Workflow == nil || cfg.LLM == nil || cfg.Similarity == nil {
		return nil, errors.New("similarity fixture requires an explicit live workflow")
	}
	if cfg.Similarity.Endpoint != FixtureSimilarityEndpoint || cfg.Similarity.APIKeyEnv != fixtureSimilarityKeyEnv || cfg.Similarity.ProviderIdentity != "fixture" || cfg.Similarity.ServiceIdentity != "local-fixture-only" {
		return nil, errors.New("fixture mode requires the local fixture endpoint and identities")
	}
	if cfg.LLM.APIKeyEnv == fixtureSimilarityKeyEnv {
		return nil, errors.New("model and fixture credentials must use different environment variables")
	}
	fixture, err := newLocalSimilarityFixture()
	if err != nil {
		return nil, err
	}
	server, err := newWithCapacity(ctx, cfg, capacity, fixture)
	if err != nil {
		_ = fixture.Close()
		return nil, err
	}
	return server, nil
}

func newLocalSimilarityFixture() (*localSimilarityFixture, error) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"provider_identity":"fixture","hits":[{"source":"fixture","external_id":"one","score":0.2}],"usage":{"input_tokens":2,"output_tokens":3,"cost_micro_usd":11}}`)
	}))
	transport := server.Client().Transport.(*http.Transport).Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "93.184.216.34:443" {
			return nil, errors.New("unexpected local similarity fixture destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	fixture := &localSimilarityFixture{client: &http.Client{Transport: transport}, server: server, transport: transport}
	fixture.oldKey, fixture.hadKey = os.LookupEnv(fixtureSimilarityKeyEnv)
	if err := os.Setenv(fixtureSimilarityKeyEnv, "fixture-only"); err != nil {
		server.Close()
		transport.CloseIdleConnections()
		return nil, err
	}
	return fixture, nil
}

func (f *localSimilarityFixture) Close() error {
	if f == nil {
		return nil
	}
	f.once.Do(func() {
		f.transport.CloseIdleConnections()
		f.server.Close()
		if os.Getenv(fixtureSimilarityKeyEnv) == "fixture-only" {
			if f.hadKey {
				f.err = os.Setenv(fixtureSimilarityKeyEnv, f.oldKey)
			} else {
				f.err = os.Unsetenv(fixtureSimilarityKeyEnv)
			}
		}
	})
	return f.err
}
