package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debugc-clis/diskc/internal/proc"
	"github.com/debugc-clis/diskc/internal/report"
)

func TestLightModeDefaults(t *testing.T) {
	mode, args := parseMode([]string{"light", "/var", "--json"})
	if mode != lightMode || len(args) != 2 || args[0] != "/var" || args[1] != "--json" {
		t.Fatalf("unexpected light mode parse: mode=%q args=%#v", mode, args)
	}
	config := defaultScanConfig(mode)
	if config.top != 10 || config.depth != 2 || config.sample != 0 || config.inspectProcesses {
		t.Fatalf("unexpected light mode defaults: %+v", config)
	}
}

func TestStandardModeDefaults(t *testing.T) {
	mode, args := parseMode([]string{"/var"})
	if mode != standardMode || len(args) != 1 || args[0] != "/var" {
		t.Fatalf("unexpected standard mode parse: mode=%q args=%#v", mode, args)
	}
	config := defaultScanConfig(mode)
	if config.top != 20 || config.depth != 4 || config.sample != 3*time.Second || !config.inspectProcesses {
		t.Fatalf("unexpected standard mode defaults: %+v", config)
	}
}

func TestRemoveLegacyDeletedFlag(t *testing.T) {
	args := removeLegacyArgs([]string{"/var", "--deleted", "--top", "5"})
	if len(args) != 3 || args[0] != "/var" || args[1] != "--top" || args[2] != "5" {
		t.Fatalf("unexpected arguments: %#v", args)
	}
}

func TestDeletedFilesAreScopedByDevice(t *testing.T) {
	files := []proc.DeletedFile{{Path: "/a", Device: 1}, {Path: "/b", Device: 2}}
	result := deletedForDevice(files, 2)
	if len(result) != 1 || result[0].Path != "/b" {
		t.Fatalf("unexpected deleted files: %#v", result)
	}
}

func TestNormalizeRootArgsSupportsInterspersedFlags(t *testing.T) {
	got := normalizeRootArgs([]string{"--top", "1", ".", "--json", "--sample", "0"})
	want := []string{"--top", "1", "--json", "--sample", "0", "."}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("normalizeRootArgs() = %#v, want %#v", got, want)
	}
}

func TestRunMixedFlagsProducesJSON(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.log"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--top", "1", root, "--json", "--sample", "0"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run returned %d, stderr=%s", code, stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("expected JSON output: %v\n%s", err, stdout.String())
	}
	files, ok := result["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("expected one file: %#v", result["files"])
	}
}

func TestRunSymlinkRootReportsTargetFiles(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "data")
	alias := filepath.Join(parent, "data-link")
	outside := filepath.Join(parent, "outside")
	for _, directory := range []string{filepath.Join(root, "nested"), outside} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rootFile := filepath.Join(root, "app.log")
	deepFile := filepath.Join(root, "nested", "deep.log")
	for _, path := range []string{rootFile, deepFile, filepath.Join(outside, "outside.log")} {
		if err := os.WriteFile(path, make([]byte, 8192), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{alias: root, filepath.Join(root, "escape"): outside, filepath.Join(root, "linked.log"): rootFile} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		depth string
		want  []string
	}{
		{depth: "0", want: []string{rootFile}},
		{depth: "1", want: []string{rootFile, deepFile}},
	} {
		t.Run("depth="+test.depth, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := []string{"light", alias, "--depth", test.depth, "--json"}
			if code := run(args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
				t.Fatalf("run returned %d, stderr=%q", code, stderr.String())
			}
			var result report.Data
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
			}
			if result.Filesystem.Path != alias {
				t.Fatalf("filesystem path = %q, want requested path %q", result.Filesystem.Path, alias)
			}
			if len(result.Files) != len(test.want) {
				t.Fatalf("unexpected files: %+v", result.Files)
			}
			seen := make(map[string]bool)
			var allocated int64
			for _, file := range result.Files {
				seen[file.Path] = true
				allocated += file.AllocatedBytes
			}
			for _, path := range test.want {
				if !seen[path] {
					t.Fatalf("missing canonical file %q: %+v", path, result.Files)
				}
			}
			foundRoot := false
			for _, directory := range result.Directories {
				if !strings.HasPrefix(directory.Path, root) {
					t.Fatalf("noncanonical directory: %+v", directory)
				}
				if directory.Path == root {
					foundRoot = true
					if directory.AllocatedBytes != allocated {
						t.Fatalf("root total = %d, want %d", directory.AllocatedBytes, allocated)
					}
				}
			}
			if !foundRoot || len(result.Warnings) != 0 {
				t.Fatalf("unexpected scan coverage: directories=%+v warnings=%v", result.Directories, result.Warnings)
			}
		})
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"light", alias, "--depth", "0"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("text run returned %d, stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"Largest files", rootFile, "Largest scanned directories", root, alias} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("text output missing %q: %s", want, stdout.String())
		}
	}
	for _, excluded := range []string{deepFile, "outside.log", "linked.log", "No regular files found"} {
		if strings.Contains(stdout.String(), excluded) {
			t.Fatalf("text output unexpectedly contains %q: %s", excluded, stdout.String())
		}
	}
}

func TestRunInvalidSymlinkRootsFail(t *testing.T) {
	parent := t.TempDir()
	for _, test := range []struct {
		name   string
		target string
	}{
		{name: "broken", target: "missing"},
		{name: "loop", target: "loop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(parent, test.name)
			if err := os.Symlink(test.target, root); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"light", root, "--json"}, &stdout, &stderr); code != 1 {
				t.Fatalf("run returned %d, want runtime failure; stderr=%q", code, stderr.String())
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), root) || !strings.Contains(stderr.String(), "no filesystems could be inspected") {
				t.Fatalf("unexpected failure output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunHelpAndUsageExitCodes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help returned %d", code)
	}
	if !strings.Contains(stdout.String(), "Usage: diskc") || stderr.Len() != 0 {
		t.Fatalf("help output misplaced: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"list", "unexpected"}, &stdout, &stderr); code != 2 {
		t.Fatalf("usage error returned %d, stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"--top", "1001"}, &stdout, &stderr); code != 2 {
		t.Fatalf("oversized top returned %d, stderr=%q", code, stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version returned %d: %s", code, stderr.String())
	}
	for _, expected := range []string{"diskc ", "commit:", "go:", "platform:"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("missing %q in %q", expected, stdout.String())
		}
	}
}

func TestRunDocHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"doc", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doc help returned %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: diskc doc") || stderr.Len() != 0 {
		t.Fatalf("unexpected doc help: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestWatchJSONIsNDJSON(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout bytes.Buffer
	scans := 0
	scan := func(writer, _ io.Writer, _ []string, config scanConfig) error {
		if !config.jsonStream {
			t.Fatal("watch JSON must enable stream mode")
		}
		scans++
		fmt.Fprintf(writer, "{\"scan\":%d}\n", scans)
		if scans == 2 {
			cancel()
		}
		return nil
	}
	if err := watchLoop(ctx, &stdout, io.Discard, []string{"/"}, scanConfig{jsonOutput: true}, time.Millisecond, false, scan); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two NDJSON records: %q", stdout.String())
	}
	for _, line := range lines {
		var value map[string]int
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatalf("invalid NDJSON line %q: %v", line, err)
		}
	}
}

func TestWatchRedirectedTextDoesNotClearScreen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var stdout bytes.Buffer
	scan := func(writer, _ io.Writer, _ []string, _ scanConfig) error {
		cancel()
		_, err := fmt.Fprintln(writer, "body")
		return err
	}
	if err := watchLoop(ctx, &stdout, io.Discard, []string{"/"}, scanConfig{}, time.Millisecond, false, scan); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "\033[") || !strings.Contains(stdout.String(), "--- diskc refresh") {
		t.Fatalf("unexpected redirected watch output: %q", stdout.String())
	}
}

func TestWatchWaitsAfterCompletedScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	scans := 0
	scan := func(_, _ io.Writer, _ []string, _ scanConfig) error {
		time.Sleep(25 * time.Millisecond)
		scans++
		if scans == 2 {
			cancel()
		}
		return nil
	}
	started := time.Now()
	if err := watchLoop(ctx, io.Discard, io.Discard, []string{"/"}, scanConfig{jsonOutput: true}, 20*time.Millisecond, false, scan); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 65*time.Millisecond {
		t.Fatalf("watch did not delay after scan completion: %s", elapsed)
	}
}
