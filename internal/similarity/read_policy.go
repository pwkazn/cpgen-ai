package similarity

// ReadPolicy owns the normalized projection and response policy without an
// HTTP client or any dispatch method. Its digests match the physical adapter.
type ReadPolicy struct{ adapter *HTTPAdapter }

func NewReadPolicy(config Config) (*ReadPolicy, error) {
	normalized, endpoint, err := config.normalized()
	if err != nil {
		return nil, err
	}
	return &ReadPolicy{&HTTPAdapter{config: normalized, endpoint: endpoint}}, nil
}
func (p *ReadPolicy) PlanSearch(request Request) (PhysicalSearchPlan, error) {
	return p.adapter.PlanSearch(request)
}
func (p *ReadPolicy) ValidatePhysicalResponse(request Request, evidence Evidence) error {
	return p.adapter.ValidatePhysicalResponse(request, evidence)
}
