package integration_test

import (
	"io"
	"strings"
	"testing"
)

// Subprocess stderr uses io.Copy. Exercise its ReaderFrom fast-path selection
// while diagnostics are read, rather than testing only direct Write calls.
func TestSubprocessDiagnosticsRemainSynchronizedDuringCopy(t *testing.T) {
	var output synchronizedBuffer
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	finished := make(chan error, 1)
	go func() { _, err := io.Copy(&output, reader); finished <- err }()
	writes := make(chan error, 1)
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := writer.Write([]byte("diagnostic\n")); err != nil {
				writes <- err
				return
			}
		}
		writes <- writer.Close()
	}()
	for {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
			if err := <-writes; err != nil {
				t.Fatal(err)
			}
			if got := output.String(); got != strings.Repeat("diagnostic\n", 200) {
				t.Fatalf("copied diagnostics were lost: %d bytes", len(got))
			}
			return
		default:
			_ = output.String()
		}
	}
}
