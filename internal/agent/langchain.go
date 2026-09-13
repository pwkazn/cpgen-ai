package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"github.com/tmc/langchaingo/llms"
	langchainopenai "github.com/tmc/langchaingo/llms/openai"
)

// LangChain uses SDK serialization while CPGen owns physical dispatch,
// credentials, endpoint policy, accounting and strict response validation.
type LangChain struct{ core *OpenAICompatible }

var _ port.MeteredLLM = (*LangChain)(nil)

// NewLangChain performs no network or credential I/O.
func NewLangChain(config Config) (*LangChain, error) {
	core, err := New(config)
	if err != nil {
		return nil, err
	}
	model := &LangChain{core: core}
	core.prepareBody = model.prepareBody
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

// Serialize before admission so token bounds account for actual SDK bytes.
// This capture performs no I/O and needs no credential. Model-specific roles,
// sampling omissions, token field names and additional fields are retained.
func (a *LangChain) prepareBody(body []byte) ([]byte, error) {
	var wire chatRequest
	if json.Unmarshal(body, &wire) != nil || len(wire.Messages) != 2 || wire.MaxTokens > int64(^uint(0)>>1) {
		return nil, &Error{Code: ErrorConfiguration}
	}
	capture := &langChainRequestCapture{maxBytes: int64(len(body)) + (1 << 20)}
	model, err := langchainopenai.New(
		langchainopenai.WithBaseURL(strings.TrimSuffix(a.core.endpoint.String(), a.core.config.CompletionPath)),
		langchainopenai.WithModel(wire.Model),
		langchainopenai.WithToken("local-serialization-only"),
		langchainopenai.WithOrganization(""),
		langchainopenai.WithHTTPClient(capture),
		langchainopenai.WithResponseFormat(langchainopenai.ResponseFormatJSON),
	)
	if err != nil {
		return nil, &Error{Code: ErrorConfiguration}
	}
	_, _ = model.GenerateContent(context.Background(), []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, wire.Messages[0].Content),
		llms.TextParts(llms.ChatMessageTypeHuman, wire.Messages[1].Content),
	}, llms.WithTemperature(wire.Temperature), llms.WithTopP(wire.TopP), llms.WithMaxTokens(int(wire.MaxTokens)))
	if !capture.called.Load() || capture.err != nil || len(capture.body) == 0 {
		return nil, &Error{Code: ErrorConfiguration}
	}
	return capture.body, nil
}

var errLangChainRequestCaptured = errors.New("SDK request captured without dispatch")

type langChainRequestCapture struct {
	called   atomic.Bool
	maxBytes int64
	body     []byte
	err      error
}

func (c *langChainRequestCapture) Do(request *http.Request) (*http.Response, error) {
	if !c.called.CompareAndSwap(false, true) {
		c.err = &Error{Code: ErrorPolicy, ConfirmedNoSend: true}
		return nil, c.err
	}
	if request == nil || request.Body == nil || request.Method != http.MethodPost {
		c.err = &Error{Code: ErrorConfiguration}
		return nil, c.err
	}
	defer request.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(request.Body, c.maxBytes+1))
	if err != nil || int64(len(raw)) > c.maxBytes || !json.Valid(raw) {
		c.err = &Error{Code: ErrorConfiguration}
		return nil, c.err
	}
	c.body = raw
	return nil, errLangChainRequestCaptured
}
