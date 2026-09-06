package application_test

import (
	"context"
	"path/filepath"
	"testing"
)

import (
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/config"
)

func TestBootstrapBuildsFakeOnlyApplicationAndCloseIsIdempotent(t *testing.T) {
	application.RegisterStorageFactory(func(ctx context.Context, cfg config.Config) (application.StorageResources, error) {
		effective, err := cfg.EffectiveConfig()
		if err != nil {
			return application.StorageResources{}, err
		}
		store, err := sqlite.Open(ctx, sqlite.Config{Path: effective.Paths.Database, BusyTimeout: cfg.SQLite.BusyTimeout, MaxReaders: cfg.SQLite.MaxReaders})
		if err != nil {
			return application.StorageResources{}, err
		}
		blobs, err := blob.NewStore(effective.Paths.Artifacts)
		if err != nil {
			_ = store.Close()
			return application.StorageResources{}, err
		}
		return application.StorageResources{Runtime: store, Reviews: store, Metadata: store, Ledger: store, BlobStore: blobs, SandboxRuntime: store, Close: store.Close}, nil
	})
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.Join(t.TempDir(), "state") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := application.Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if app.Runs == nil || app.Runtime == nil || app.Reviews == nil || app.Maintenance == nil || app.Locks == nil {
		t.Fatal("bootstrap returned an incomplete application")
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal("second Close is not idempotent: ", err)
	}
}
