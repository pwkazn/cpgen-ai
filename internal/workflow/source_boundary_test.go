package workflow_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceBoundaryContainsNoRuntimeAssembly(t *testing.T) {
	for _, root := range []string{"../../internal/workflow", "../../internal/adapter/fake"} {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			source := string(data)
			for _, forbidden := range []string{"internal/adapter/storage/sqlite", "internal/runlock", "Services", "map[string]any", "[]Step["} {
				if strings.Contains(source, forbidden) {
					t.Fatalf("%s contains forbidden %q", entry.Name(), forbidden)
				}
			}
		}
	}
}
