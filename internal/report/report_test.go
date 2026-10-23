package report

import (
	"bytes"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/health"
)

func TestPrintJSON(t *testing.T) {
	var output bytes.Buffer
	data := Data{Filesystem: disk.Filesystem{Path: "/", FreeBytes: 1024, AvailableBytes: 1024}, Files: []disk.File{{Path: "/tmp/app.log", AllocatedBytes: 2048, ApparentBytes: 2048, Kind: "log"}}}
	if err := Print(&output, data, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"/tmp/app.log"`) || !strings.Contains(output.String(), `"allocated_bytes": 2048`) {
		t.Fatalf("unexpected JSON: %s", output.String())
	}
}

func TestPrintIncludesGrowthAndDiagnostics(t *testing.T) {
	var output bytes.Buffer
	data := Data{
		Filesystem: disk.Filesystem{Path: "/", FreeBytes: 1024},
		Files:      []disk.File{{Path: "/large.log", AllocatedBytes: 2048, ApparentBytes: 2048, Kind: "log"}},
		Growing:    []disk.File{{Path: "/small-fast.log", AllocatedBytes: 10, ApparentBytes: 10, Kind: "log", Growth: 512}},
		Findings:   []health.Finding{{Severity: "warning", Kind: "device-io", Message: "sda: 99% busy"}},
	}
	if err := Print(&output, data, false); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{"Fastest growth", "/small-fast.log", "System diagnostics", "sda: 99% busy"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("expected %q in output: %s", expected, text)
		}
	}
}

func TestPrintSummarizesWarningsAndShowsShrinkage(t *testing.T) {
	var output bytes.Buffer
	warnings := []string{"one", "two", "three", "four", "five", "six", "seven"}
	data := Data{
		Filesystem: disk.Filesystem{Path: "/", FreeBytes: 1024},
		Files:      []disk.File{{Path: "/shrinking.log", AllocatedBytes: 2048, ApparentBytes: 2048, Kind: "log", Growth: -512}},
		Warnings:   warnings,
	}
	if err := Print(&output, data, false); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{"↓ 512 B/s", "2 more warnings omitted", "Largest files"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("expected %q in output: %s", expected, text)
		}
	}
	if strings.Contains(text, "- six") || strings.Contains(text, "- seven") {
		t.Fatalf("expected warning details to be capped: %s", text)
	}
}

func TestPrintLabelsLightMode(t *testing.T) {
	var output bytes.Buffer
	data := Data{Mode: "light", Filesystem: disk.Filesystem{Path: "/", FreeBytes: 1024}}
	if err := Print(&output, data, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Light mode: process checks skipped") {
		t.Fatalf("expected light mode label: %s", output.String())
	}

	output.Reset()
	if err := Print(&output, data, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"mode": "light"`) {
		t.Fatalf("expected light mode in JSON: %s", output.String())
	}
}

func TestPrintEscapesTerminalControls(t *testing.T) {
	var output bytes.Buffer
	data := Data{
		Filesystem: disk.Filesystem{Path: "/safe\n\x1b[2J"},
		Files:      []disk.File{{Path: "/tmp/bad\tname", AllocatedBytes: 1, Kind: "log"}},
		Warnings:   []string{"warning\rnext"},
	}
	if err := Print(&output, data, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "\x1b") || !strings.Contains(output.String(), `\x1b`) || !strings.Contains(output.String(), `bad\tname`) {
		t.Fatalf("control characters were not escaped: %q", output.String())
	}
}

func TestPrintReturnsWriterError(t *testing.T) {
	err := Print(errorWriter{}, Data{Filesystem: disk.Filesystem{Path: "/"}}, false)
	if !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("expected EPIPE, got %v", err)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, syscall.EPIPE }
