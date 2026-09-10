package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"github.com/tmc/langchaingo/llms"
	langchainopenai "github.com/tmc/langchaingo/llms/openai"
)

// LangChain implements MeteredLLM using LangChainGo's OpenAI-compatible
// client. It shares CPGen's strict schema, endpoint, identity and retry
// contracts. Application code must use one attempt per durable dispatch;
// the standalone Generate trace is not a substitute for the call ledger.
type LangChain struct{ core *OpenAICompatible }

var _ port.MeteredLLM = (*LangChain)(nil)

// NewLangChain performs no network or credential I/O. Config contains only
// a credential environment reference; the key is read at dispatch time.
func NewLangChain(config Config) (*LangChain, error) {
	core, err := New(config)
	if err != nil {
		return nil, err
	}
	model := &LangChain{core: core}
	core.exchange = model.exchange
	return model, nil
}

func (a *LangChain) Generate(ctx context.Context, request port.GenerateRequest) (domain.MeteredOutcome[port.GenerateResponse], error) {
	if a == nil || a.core == nil {
		return domain.MeteredOutcome[port.GenerateResponse]{}, &Error{Code: ErrorConfiguration}
	}
	outcome, err := a.core.Generate(ctx, request)
	if outcome.Value != nil {
		outcome.Value.ProviderMeta["adapter"] = "langchaingo-openai-v1"
	}
	return outcome, err
}

func (a *LangChain) exchange(ctx context.Context, body []byte, key, identity string) ([]byte, int, string, time.Duration, bool, error) {
	var wire chatRequest
	if json.Unmarshal(body, &wire) != nil || wire.MaxTokens > int64(^uint(0)>>1) {
		return nil, 0, "", 0, false, &Error{Code: ErrorConfiguration, ConfirmedNoSend: true}
	}
	doer := &langChainDoer{core: a.core, expected: wire, key: key, identity: identity}
	model, err := langchainopenai.New(
		langchainopenai.WithBaseURL(strings.TrimSuffix(a.core.endpoint.String(), a.core.config.CompletionPath)),
		langchainopenai.WithModel(wire.Model),
		langchainopenai.WithToken(key),
		langchainopenai.WithOrganization(""),
		langchainopenai.WithHTTPClient(doer),
		langchainopenai.WithResponseFormat(langchainopenai.ResponseFormatJSON),
	)
	if err != nil {
		return nil, 0, "", 0, false, &Error{Code: ErrorConfiguration, ConfirmedNoSend: true}
	}
	// No callbacks, tools, streaming, library retry transport or ambient
	// organization settings. The doer enforces exactly one physical send.
	_, _ = model.GenerateContent(ctx, []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, wire.Messages[0].Content),
		llms.TextParts(llms.ChatMessageTypeHuman, wire.Messages[1].Content),
	}, llms.WithTemperature(wire.Temperature), llms.WithTopP(wire.TopP),
		llms.WithMaxTokens(int(wire.MaxTokens)), langchainopenai.WithLegacyMaxTokensField())
	if !doer.called.Load() {
		// Library errors can include private provider data and lose context
		// identity. They never cross the adapter boundary.
		return nil, 0, "", 0, false, &Error{Code: ErrorConfiguration, ConfirmedNoSend: true}
	}
	// CPGen validates the original bounded bytes, not the SDK's lossy DTO:
	// otherwise duplicate fields, missing usage and malformed UTF-8 disappear.
	return doer.body, doer.status, doer.providerID, doer.retryAfter, doer.sent, doer.err
}

// langChainDoer exists for a single GenerateContent invocation. The SDK's
// request is checked and canonicalized before the shared policy transport
// sends it. Captured response bytes live only until strict local decoding.
type langChainDoer struct {
	core          *OpenAICompatible
	expected      chatRequest
	key, identity string
	called        atomic.Bool
	body          []byte
	status        int
	providerID    string
	retryAfter    time.Duration
	sent          bool
	err           error
}

func (d *langChainDoer) Do(request *http.Request) (*http.Response, error) {
	if !d.called.CompareAndSwap(false, true) {
		return nil, &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
	}
	d.err = &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
	if request == nil || request.Body == nil || request.Method != http.MethodPost {
		return nil, d.err
	}
	defer request.Body.Close()
	var generated struct {
		chatRequest
		Temperature *float64 `json:"temperature"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&generated) != nil || generated.Temperature == nil {
		return nil, d.err
	}
	actual := generated.chatRequest
	actual.Temperature = *generated.Temperature
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return nil, d.err
	}
	// v0.1.14 drops top_p in GenerateContent. Restore exactly the admitted
	// value. Reject any other SDK semantic rewrite (including system roles).
	actual.TopP = d.expected.TopP
	if !reflect.DeepEqual(actual, d.expected) {
		return nil, d.err
	}
	canonical, err := json.Marshal(actual)
	if err != nil {
		return nil, d.err
	}
	d.body, d.status, d.providerID, d.retryAfter, d.sent, d.err = d.core.doHTTPRequest(request.Context(), canonical, d.key, d.identity)
	if d.err != nil {
		return nil, d.err
	}
	// The SDK dereferences choice pointers and legacy function-call payloads.
	// Keep unsupported/non-complete choices out of its decoder, while leaving
	// final acceptance to CPGen's strict raw-response validator.
	var envelope struct {
		Choices []*struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(d.body, &envelope) == nil {
		for _, choice := range envelope.Choices {
			if choice == nil || (choice.FinishReason != "" && choice.FinishReason != "stop") {
				return nil, &Error{Code: ErrorProtocol}
			}
		}
	}
	return &http.Response{StatusCode: d.status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(d.body))}, nil
}
