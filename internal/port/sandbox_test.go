package port_test

import (
	"encoding/json"
	"testing"

	"cpgen/internal/port"
)

func TestPortEnumsRejectUnknownJSONValues(t *testing.T) {
	t.Parallel()
	for _, target := range []any{new(port.Language), new(port.ProgramRole), new(port.ContainerRole)} {
		if err := json.Unmarshal([]byte(`"UNKNOWN"`), target); err == nil {
			t.Fatalf("%T accepted an unknown enum value", target)
		}
	}
}
