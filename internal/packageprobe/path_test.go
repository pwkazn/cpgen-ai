package packageprobe

import (
	"testing"

	"cpgen/internal/domain"
)

func TestPackagePathsRejectTraversalControlsAndCrossPlatformCollisions(t *testing.T) {
	for _, unsafe := range []string{
		"", "../escape", "/absolute", "C:\\drive", "nested\\backslash", "a/../b",
		"nul\x00byte", "control\x1fbyte", "line\nbreak", ".", "a//b",
	} {
		if err := validatePackagePaths([]domain.SafeRelPath{domain.SafeRelPath(unsafe)}, 240); err == nil {
			t.Fatalf("unsafe package path %q was accepted", unsafe)
		}
	}
	for _, collision := range [][]domain.SafeRelPath{
		{"Statement/A.md", "statement/a.md"},
		{"statement/é.md", "statement/e\u0301.md"},
	} {
		if err := validatePackagePaths(collision, 240); err == nil {
			t.Fatalf("cross-platform collision %#v was accepted", collision)
		}
	}
	if err := validatePackagePaths([]domain.SafeRelPath{"statement/题面.md", "tests/001.in"}, 240); err != nil {
		t.Fatal(err)
	}
}
