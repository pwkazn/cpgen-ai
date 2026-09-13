package similarity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestPackageSafeProjectionAndRequestAreCanonical(t *testing.T) {
	projection, err := NewPackageSafeProjection(" e\u0301 题 ", "line one\r\nline two", []string{"z", "a", "a"}, "CPP")
	if err != nil {
		t.Fatal(err)
	}
	if projection.NormalizedTitle != "é 题" || projection.NormalizedStatement != "line one\nline two" || projection.Language != "cpp" || strings.Join(projection.NormalizedTags, ",") != "a,z" {
		t.Fatalf("projection normalization = %#v", projection)
	}
	policy, err := NewPolicy("similarity/v1", .5, .8, 0, 0, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewRequest(projection, policy.PolicyRef, policy.PolicyDigest, "sim-test-1")
	if err != nil {
		t.Fatal(err)
	}
	one, err := request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	two, err := request.CanonicalJSON()
	if err != nil || string(one) != string(two) {
		t.Fatalf("canonical request changed: %s / %s (%v)", one, two, err)
	}
	if !strings.Contains(string(one), "candidate_projection_digest") || strings.Contains(string(one), "solution") {
		t.Fatalf("unsafe request projection: %s", one)
	}
	request.NormalizedTags[0] = "mutated"
	if request.Projection().NormalizedTags[0] != "mutated" {
		t.Fatal("request projection copy expectation failed")
	}
}

func TestSortHitsDeterministicAndDefensive(t *testing.T) {
	first, err := NewHit("z-source", "id-z", "z title", .5, "https://example.test/z")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewHit("a-source", "id-a", "a title", .5, "https://example.test/a")
	if err != nil {
		t.Fatal(err)
	}
	strong, err := NewHit("z-source", "id-strong", "strong", .9, "https://example.test/strong")
	if err != nil {
		t.Fatal(err)
	}
	input := []Hit{first, second, strong}
	sorted, err := SortHits(input)
	if err != nil {
		t.Fatal(err)
	}
	if sorted[0].ExternalID != "id-strong" || sorted[1].ExternalID != "id-a" || sorted[2].ExternalID != "id-z" {
		t.Fatalf("sort = %#v", sorted)
	}
	if input[0].ExternalID != "id-z" {
		t.Fatal("SortHits mutated input")
	}
}

func TestPolicyUsesExactBoundariesAndFailsClosed(t *testing.T) {
	policy, err := NewPolicy("similarity/v1", .5, .8, 0, 0, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(t, policy)
	for _, tc := range []struct {
		name  string
		score float64
		want  DecisionKind
	}{
		{name: "below acceptance", score: .499999, want: DecisionAccept},
		{name: "accept boundary", score: .5, want: DecisionNeedsReview},
		{name: "review upper boundary", score: .8 - 0.000001, want: DecisionNeedsReview},
		{name: "reject boundary", score: .8, want: DecisionReject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := testEvidence(t, request, tc.score)
			decision := Evaluate(policy, evidence)
			if decision.Kind != tc.want {
				t.Fatalf("decision = %#v, want %s", decision, tc.want)
			}
			if err := decision.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	noHitsRequest := request
	noHits, err := NewEvidence(noHitsRequest, "fixture", nil, testTime(), Usage{InputTokens: 1, OutputTokens: 1}, CacheProvenance{Kind: CacheLive}, testTrace("no-hits"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Evaluate(policy, noHits); got.Kind != DecisionBlocked || got.ExplanationCode != ExplanationInsufficient {
		t.Fatalf("insufficient decision = %#v", got)
	}
	otherPolicy, err := NewPolicy("other/v1", .5, .8, 0, 0, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.PolicyDigest = otherPolicy.PolicyDigest
	stale, err := NewEvidence(request, "fixture", []Hit{testHit(t, .4)}, testTime(), Usage{InputTokens: 1, OutputTokens: 1}, CacheProvenance{Kind: CacheLive}, testTrace("stale"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Evaluate(policy, stale); got.Kind != DecisionBlocked || got.ExplanationCode != ExplanationPolicyMismatch {
		t.Fatalf("stale policy decision = %#v", got)
	}
}

func TestEvidenceBindsCacheProvenanceAndDigest(t *testing.T) {
	policy, err := NewPolicy("similarity/v1", .5, .8, .5, .8, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(t, policy)
	cache := CacheProvenance{Kind: CacheHit, CacheKeyDigest: domain.SumBytes([]byte("cache")), SourceEvidenceDigest: domain.SumBytes([]byte("source")), SourceCallRecordID: domain.CallRecordID("call_00000000000000000000000000000001"), CurrentCallRecordID: domain.CallRecordID("call_00000000000000000000000000000002"), SourceOccurrenceIDs: []domain.ArtifactOccurrenceID{"occ_00000000000000000000000000000001"}}
	evidence, err := NewEvidence(request, "fixture", []Hit{testHit(t, .4)}, testTime(), Usage{InputTokens: 3, OutputTokens: 4}, cache, testTrace("cache"))
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.Validate(); err != nil {
		t.Fatal(err)
	}
	if evidence.EvidenceDigest == "" {
		t.Fatal("missing evidence digest")
	}
	evidence.Cache.CurrentCallRecordID = evidence.Cache.SourceCallRecordID
	if err := evidence.Validate(); err != nil {
		// Equal source/current call IDs are not currently forbidden by the
		// provider-neutral value, but the digest must change on mutation.
		return
	}
	if evidence.EvidenceDigest == mustDigestEvidence(evidence) {
		t.Fatal("mutated evidence retained its old digest")
	}
}

func TestFakeIsDeterministicAndBindsRequests(t *testing.T) {
	policy, err := NewPolicy("fake/v1", .5, .8, .5, .8, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(t, policy)
	evidence := testEvidence(t, request, .4)
	fake := NewFakeEvidence(evidence)
	outcome, err := fake.SearchEvidence(context.Background(), request)
	if err != nil || outcome.Value == nil {
		t.Fatalf("fake outcome = %#v, err=%v", outcome, err)
	}
	if len(fake.Requests()) != 1 {
		t.Fatalf("fake requests = %#v", fake.Requests())
	}
	request.NormalizedTags = append(request.NormalizedTags, "changed")
	if _, err := fake.SearchEvidence(context.Background(), request); err == nil {
		t.Fatal("fake accepted a request bound to another evidence")
	}
}

func TestHTTPAdapterSuccessUsageSortingAndCompatibilityPort(t *testing.T) {
	const secret = "similarity-secret-never-in-errors"
	t.Setenv("CPGEN_SIM_TEST_KEY", secret)
	var gotAuth, gotID atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotID.Store(r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider_identity":"fixture","model_version":"m1","index_version":"i1","hits":[{"source":"z","external_id":"z","title":"z","score":0.5},{"source":"a","external_id":"a","title":"a","score":0.5},{"source":"a","external_id":"strong","title":"strong","score":0.9}],"usage":{"input_tokens":7,"output_tokens":9,"cost_micro_usd":11}}`))
	}))
	defer server.Close()
	adapter, err := New(testHTTPConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy("http/v1", .5, .8, .5, .8, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := NewPackageSafeProjection("title", "statement", []string{"tag"}, "go")
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewRequest(projection, policy.PolicyRef, policy.PolicyDigest, "logical-http")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := adapter.SearchEvidence(context.Background(), request)
	if err != nil || outcome.Value == nil {
		t.Fatalf("adapter outcome = %#v, err=%v", outcome, err)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := outcome.Value.Usage; got != (Usage{InputTokens: 7, OutputTokens: 9, CostMicroUSD: 11}) || outcome.Value.UsageSource != UsageProviderVerified || outcome.Value.ModelVersion != "m1" || outcome.Value.IndexVersion != "i1" {
		t.Fatalf("metadata = %#v", outcome.Value)
	}
	if outcome.Value.Hits[0].ExternalID != "strong" || gotAuth.Load().(string) != "Bearer "+secret || gotID.Load().(string) == "" {
		t.Fatalf("request metadata/hits = %#v auth=%q id=%q", outcome.Value.Hits, gotAuth.Load(), gotID.Load())
	}
	portOutcome, err := adapter.Search(context.Background(), port.SimilaritySearchRequest{Query: "statement", QueryDigest: domain.SumBytes([]byte("statement")), Limit: 3})
	if err != nil || portOutcome.Value == nil {
		var typed *Error
		if errors.As(err, &typed) {
			t.Fatalf("compat outcome = %#v, err=%v cause=%v", portOutcome, err, typed.cause)
		}
		t.Fatalf("compat outcome = %#v, err=%v", portOutcome, err)
	}
	if err := portOutcome.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := string(portOutcome.Value.ResponseMetadata["usage_source"]); got != `"provider_verified"` {
		t.Fatalf("compat usage metadata = %s", got)
	}
}

func TestHTTPAdapterRefusesRedirectAndResponseOverflow(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	t.Setenv("CPGEN_SIM_TEST_KEY", "secret")
	adapter, err := New(testHTTPConfig(redirect.URL))
	if err != nil {
		t.Fatal(err)
	}
	request := testRequestWithAdapterPolicy(t)
	outcome, err := adapter.SearchEvidence(context.Background(), request)
	if err != nil || outcome.Failure == nil || outcome.Failure.Code != domain.FailureProtocol || redirected.Load() != 0 {
		t.Fatalf("redirect outcome=%#v err=%v redirected=%d", outcome, err, redirected.Load())
	}
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 100))) }))
	defer large.Close()
	config := testHTTPConfig(large.URL)
	config.MaxResponseBytes = 16
	adapter, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = adapter.SearchEvidence(context.Background(), request)
	if err != nil || outcome.Failure == nil || outcome.Failure.Code != domain.FailureProtocol {
		t.Fatalf("overflow outcome=%#v err=%v", outcome, err)
	}
}

func TestHTTPAdapterStrictJSONHTTPSAndHostAllowlist(t *testing.T) {
	t.Setenv("CPGEN_SIM_TEST_KEY", "secret")
	for _, body := range []string{
		`{"hits":[],"unknown":1}`,
		`{"hits":[],"hits":[]}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		adapter, err := New(testHTTPConfig(server.URL))
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		outcome, callErr := adapter.SearchEvidence(context.Background(), testRequestWithAdapterPolicy(t))
		server.Close()
		if callErr != nil || outcome.Failure == nil || outcome.Failure.Code != domain.FailureProtocol {
			t.Fatalf("strict response %s: outcome=%#v err=%v", body, outcome, callErr)
		}
	}
	if _, err := New(Config{Endpoint: "https://allowed.example", APIKeyEnv: "CPGEN_SIM_TEST_KEY", AllowedHosts: []string{"other.example"}}); err == nil {
		t.Fatal("host allowlist did not reject endpoint")
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hits":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer tlsServer.Close()
	config := testHTTPConfig(tlsServer.URL)
	config.AllowInsecureHTTP = false
	config.AllowLoopbackForTesting = true
	config.HTTPClient = tlsServer.Client()
	adapter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := adapter.SearchEvidence(context.Background(), testRequestWithAdapterPolicy(t))
	if err != nil || outcome.Value == nil {
		t.Fatalf("TLS outcome=%#v err=%v", outcome, err)
	}
}

func TestHTTPAdapterPolicyCredentialCancellationAndRetry(t *testing.T) {
	if _, err := New(Config{Endpoint: "https://example.test", APIKeyEnv: "CPGEN_SIM_TEST_KEY"}); err != nil {
		t.Fatalf("valid HTTPS endpoint rejected: %v", err)
	}
	if _, err := New(Config{Endpoint: "http://example.test", APIKeyEnv: "CPGEN_SIM_TEST_KEY", AllowInsecureHTTP: true}); err == nil {
		t.Fatal("accepted non-loopback insecure endpoint")
	}
	stopBlockedHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stopBlockedHandler:
		}
	}))
	defer func() {
		close(stopBlockedHandler)
		server.Close()
	}()
	t.Setenv("CPGEN_SIM_TEST_KEY", "secret")
	config := testHTTPConfig(server.URL)
	config.Timeout = 30 * time.Millisecond
	adapter, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequestWithAdapterPolicy(t)
	outcome, err := adapter.SearchEvidence(context.Background(), request)
	if err != nil || outcome.Failure == nil || outcome.Failure.Class != domain.FailureUnknown {
		t.Fatalf("timeout outcome=%#v err=%v", outcome, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.SearchEvidence(ctx, request); err == nil {
		t.Fatal("pre-canceled request did not return typed error")
	} else {
		var typed *Error
		if !errors.As(err, &typed) || typed.Code != ErrorCanceled || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %T %v", err, err)
		}
	}
	var attempts atomic.Int32
	retryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"hits":[{"source":"fixture","external_id":"id","title":"title","score":0.2}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer retryServer.Close()
	config = testHTTPConfig(retryServer.URL)
	config.MaxAttempts, config.RetryBaseDelay, config.RetryMaxDelay = 2, time.Nanosecond, time.Millisecond
	adapter, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = adapter.SearchEvidence(context.Background(), request)
	if err != nil || outcome.Value == nil || attempts.Load() != 2 || len(outcome.Value.CallTrace.PhysicalAttemptCallIDs) != 2 {
		t.Fatalf("retry outcome=%#v err=%v attempts=%d", outcome, err, attempts.Load())
	}
}

func TestHTTPAdapterRedactsProviderContentAndMissingCredentialIsBlocked(t *testing.T) {
	const secret = "body-secret-must-not-escape"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(secret))
	}))
	defer server.Close()
	t.Setenv("CPGEN_SIM_TEST_KEY", "secret")
	adapter, err := New(testHTTPConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := adapter.SearchEvidence(context.Background(), testRequestWithAdapterPolicy(t))
	if err != nil || outcome.Failure == nil || strings.Contains(string(outcome.Failure.Code), secret) {
		t.Fatalf("redaction outcome=%#v err=%v", outcome, err)
	}
	t.Setenv("CPGEN_SIM_TEST_KEY", "")
	outcome, err = adapter.SearchEvidence(context.Background(), testRequestWithAdapterPolicy(t))
	if err != nil || outcome.Failure == nil || outcome.Failure.Class != domain.FailureBlocked || outcome.CallTrace.DispatchKind != domain.DispatchNone {
		t.Fatalf("missing credential outcome=%#v err=%v", outcome, err)
	}
}

func testTime() time.Time { return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) }

func testPolicy(t *testing.T) DecisionPolicy {
	t.Helper()
	policy, err := NewPolicy("test/v1", .5, .8, .5, .8, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func testRequest(t *testing.T, policy DecisionPolicy) Request {
	t.Helper()
	projection, err := NewPackageSafeProjection("title", "statement", []string{"tag"}, "go")
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewRequest(projection, policy.PolicyRef, policy.PolicyDigest, "fake-logical")
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func testRequestWithAdapterPolicy(t *testing.T) Request {
	return testRequest(t, testPolicy(t))
}

func testHit(t *testing.T, score float64) Hit {
	t.Helper()
	hit, err := NewHit("fixture", "id", "title", score, "https://example.test/item")
	if err != nil {
		t.Fatal(err)
	}
	return hit
}

func testTrace(name string) domain.CallTrace {
	return domain.CallTrace{LogicalOperationID: "similarity-test-" + name, DispatchKind: domain.DispatchNone}
}

func testEvidence(t *testing.T, request Request, score float64) Evidence {
	t.Helper()
	evidence, err := NewEvidence(request, "fixture", []Hit{testHit(t, score)}, testTime(), Usage{InputTokens: 1, OutputTokens: 1}, CacheProvenance{Kind: CacheLive}, testTrace("evidence"))
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func testHTTPConfig(endpoint string) Config {
	return Config{Endpoint: endpoint, APIKeyEnv: "CPGEN_SIM_TEST_KEY", AllowInsecureHTTP: true, Timeout: time.Second, MaxAttempts: 1, RetryBaseDelay: time.Nanosecond, RetryMaxDelay: time.Millisecond}
}

func mustDigestEvidence(e Evidence) domain.Digest {
	return digestEvidence(e)
}
