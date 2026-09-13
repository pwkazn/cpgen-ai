package agent

import "cpgen/internal/port"

// ReadPolicy reconstructs wire digests and validates saved responses. It owns
// no HTTP transport and exposes no method that can dispatch a provider call.
type ReadPolicy struct{ model *LangChain }

func NewReadPolicy(config Config) (*ReadPolicy, error) {
	normalized, endpoint, err := config.normalized()
	if err != nil {
		return nil, err
	}
	core := &OpenAICompatible{config: normalized, endpoint: endpoint}
	model := &LangChain{core: core}
	core.prepareBody = model.prepareBody
	return &ReadPolicy{model}, nil
}
func (p *ReadPolicy) PlanGenerate(request port.GenerateRequest) (port.LLMRequestPlan, error) {
	return p.model.PlanGenerate(request)
}
func (p *ReadPolicy) ValidatePhysicalResponse(request port.GenerateRequest, response port.GenerateResponse) error {
	return p.model.ValidatePhysicalResponse(request, response)
}
