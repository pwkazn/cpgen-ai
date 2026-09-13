package workflow_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// Import boundaries apply to all production files, including new nested
// packages. Library state and provider DTOs must not enter business contracts.
func TestProviderAndGraphLibraryImportsStayAtIntegrationBoundaries(t *testing.T) {
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range file.Imports {
				name, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if name == "github.com/smallnest/langgraphgo" || strings.HasPrefix(name, "github.com/smallnest/langgraphgo/") {
					t.Errorf("%s imports a graph runtime; fixed scheduling belongs to the local application loop", path)
				}
				for dependency, allowed := range map[string]string{
					"github.com/tmc/langchaingo": "../../internal/agent/",
				} {
					if (name == dependency || strings.HasPrefix(name, dependency+"/")) && !strings.HasPrefix(filepath.ToSlash(path), allowed) {
						t.Errorf("%s imports %s outside %s", path, name, allowed)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
