package textfmt

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/debugc-clis/diskc/internal/disk"
)

func TestSafeEscapesTerminalControls(t *testing.T) {
	input := "ok\nnext\t\x1b[2J\u202ename"
	want := `ok\nnext\t\x1b[2J\u202ename`
	if got := Safe(input); got != want {
		t.Fatalf("Safe(%q) = %q, want %q", input, got, want)
	}
}

func TestGrowthStatusNote(t *testing.T) {
	for _, test := range []struct{ status, want string }{
		{"", ""},
		{"measured", ""},
		{"replaced", "  [growth unmeasured: file replaced; resample]"},
		{"unavailable", "  [growth unmeasured: file unavailable or no longer regular; last observed size]"},
	} {
		if got := GrowthStatusNote(test.status); got != test.want {
			t.Errorf("GrowthStatusNote(%q) = %q; want %q", test.status, got, test.want)
		}
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

func TestFormatScanScopeExplainsBoundariesAndSafeNextSteps(t *testing.T) {
	scope := &disk.ScanScope{Root: "/data\n\x1b[31m", MaxDepth: 0, RegularFiles: 0, SkippedDepthDirectories: 2, SkippedFilesystemDirectories: 3, SkippedPseudoDirectories: 4, SkippedSymlinks: 5, InspectionErrors: 6}
	text := FormatScanScope(scope)
	for _, want := range []string{`/data\n\x1b[31m`, "max directory depth 0", "0 unique regular file(s) observed", "depth=2, other filesystem=3, pseudo filesystem=4 | symlinks=5 | inspection errors=6", "Directory totals cover observed files only", "increase --depth", "linked or mounted data paths", "review scan warnings and access permissions"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Fatalf("raw terminal escape: %q", text)
	}
	zero := FormatScanScope(&disk.ScanScope{Root: "/empty"})
	if strings.Contains(zero, "Next:") || strings.Contains(zero, "complete") || !strings.Contains(zero, "depth=0, other filesystem=0, pseudo filesystem=0 | symlinks=0 | inspection errors=0") {
		t.Fatalf("zero counts make unsupported claims: %s", zero)
	}
	if got := FormatScanScope(nil); got != "" {
		t.Fatalf("absent scan should have no scope: %q", got)
	}
}
