package application

import (
	"errors"
	"slices"
	"testing"
)

func TestApplicationClosesExecutionBeforeStorageOnceEvenOnFailure(t *testing.T) {
	var order []string
	failed := errors.New("execution close failed")
	a := &Application{
		closeExecution: func() error { order = append(order, "execution"); return failed },
		closeStorage:   func() error { order = append(order, "storage"); return nil },
	}
	for i := 0; i < 2; i++ {
		if err := a.Close(); !errors.Is(err, failed) {
			t.Fatalf("lost cleanup error: %v", err)
		}
	}
	if !slices.Equal(order, []string{"execution", "storage"}) {
		t.Fatalf("resource close order: %v", order)
	}
}
