package textfmt

import (
	"errors"
	"syscall"
	"testing"
)

func TestSafeEscapesTerminalControls(t *testing.T) {
	input := "ok\nnext\t\x1b[2J\u202ename"
	want := `ok\nnext\t\x1b[2J\u202ename`
	if got := Safe(input); got != want {
		t.Fatalf("Safe(%q) = %q, want %q", input, got, want)
	}
}

func TestWriterKeepsFirstError(t *testing.T) {
	writer := NewWriter(failingWriter{})
	if _, err := writer.Write([]byte("first")); !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("expected EPIPE, got %v", err)
	}
	if _, err := writer.Write([]byte("second")); !errors.Is(err, syscall.EPIPE) || !errors.Is(writer.Err(), syscall.EPIPE) {
		t.Fatalf("expected sticky EPIPE, got %v / %v", err, writer.Err())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, syscall.EPIPE }
