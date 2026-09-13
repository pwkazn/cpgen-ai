package fake

import (
	"encoding/json"

	"cpgen/internal/port"
)

func cloneGenerateRequest(request port.GenerateRequest) port.GenerateRequest {
	request.Variables = append(json.RawMessage(nil), request.Variables...)
	return request
}
