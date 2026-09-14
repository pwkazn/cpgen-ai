package application_test

import (
	"context"
	"path/filepath"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
)

func TestBootstrapLocalClosesPersistenceWithoutExecutors(t *testing.T) {
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.Join(t.TempDir(), "state") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := application.BootstrapLocal(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if app.Runs != nil || app.Packages == nil || app.Runtime == nil || app.Locks == nil {
		t.Fatal("invalid local composition")
	}
	if _, err := app.Runtime.ListRuns(context.Background(), domain.RunFilter{}); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
}
