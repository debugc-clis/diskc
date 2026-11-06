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

	"github.com/debugc-clis/diskc/internal/disk"
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

func TestRefreshKeepsWritersWithTheirObservedRows(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "app.log"), []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldAttribute := attributeWriters
	t.Cleanup(func() { attributeWriters = oldAttribute })
	const warning = "writer attribution partial across inspected processes"
	calls := 0
	attributeWriters = func(files []disk.File) []string {
		calls++
		if len(files) != 2 || files[0].Path != files[1].Path {
			t.Fatalf("expected two overlapping observations, got %+v", files)
		}
		// Emulate different identities observed at the shared path, with only the
		// first observation retaining a visible writable descriptor.
		files[0].Writers = []disk.Writer{{PID: 123}}
		files[1].Writers = nil
		return []string{warning}
	}
	config := defaultScanConfig(standardMode)
	config.top, config.sample, config.jsonOutput = 1, 0, true
	var stdout, stderr bytes.Buffer
	if err := refresh(&stdout, &stderr, []string{root, nested}, config); err != nil {
		t.Fatal(err)
	}
	var result []report.Data
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(result) != 2 {
		t.Fatalf("got calls=%d outputs=%d", calls, len(result))
	}
	if writers := result[0].Files[0].Writers; len(writers) != 1 || writers[0].PID != 123 {
		t.Fatalf("first observation lost writers: %+v", writers)
	}
	if writers := result[1].Files[0].Writers; len(writers) != 0 {
		t.Fatalf("second observation inherited writers: %+v", writers)
	}
	for _, output := range result {
		found := 0
		for _, got := range output.Warnings {
			if got == warning {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("each root needs the shared visibility warning once: %+v", output.Warnings)
		}
	}
}

func TestRefreshPreservesPartialProcessEvidenceAndWarnings(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.log"), []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}
	usage, err := disk.FilesystemUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	oldAttribute, oldDeleted := attributeWriters, inspectDeletedFiles
	t.Cleanup(func() { attributeWriters, inspectDeletedFiles = oldAttribute, oldDeleted })
	const writerWarning = "writer attribution partial; investigate as the affected service user"
	const deletedWarning = "deleted-open inspection partial; retained total is not exhaustive"
	attributeCalls, deletedCalls := 0, 0
	attributeWriters = func(files []disk.File) []string {
		attributeCalls++
		for index := range files {
			files[index].Writers = []disk.Writer{{PID: 123, Process: "visible-writer"}}
		}
		return []string{writerWarning}
	}
	inspectDeletedFiles = func() ([]proc.DeletedFile, []string) {
		deletedCalls++
		return []proc.DeletedFile{{Path: "/retained.log", AllocatedBytes: 8192, Device: usage.Device}}, []string{deletedWarning}
	}
	for _, mode := range []runMode{standardMode, lightMode} {
		for _, format := range []string{"text", "json", "stream"} {
			t.Run(string(mode)+"/"+format, func(t *testing.T) {
				attributeCalls, deletedCalls = 0, 0
				config := defaultScanConfig(mode)
				config.sample = 0
				config.jsonOutput, config.jsonStream = format != "text", format == "stream"
				var stdout, stderr bytes.Buffer
				if err := refresh(&stdout, &stderr, []string{root}, config); err != nil {
					t.Fatal(err)
				}
				if mode == lightMode {
					if attributeCalls != 0 || deletedCalls != 0 || strings.Contains(stdout.String(), writerWarning) || strings.Contains(stdout.String(), deletedWarning) {
						t.Fatalf("light mode inspected processes: attribute=%d deleted=%d output=%s", attributeCalls, deletedCalls, stdout.String())
					}
					return
				}
				if attributeCalls != 1 || deletedCalls != 1 {
					t.Fatalf("expected one pass each: attribute=%d deleted=%d", attributeCalls, deletedCalls)
				}
				if format == "text" {
					for _, want := range []string{writerWarning, deletedWarning, "PID 123", "Deleted but still open", "/retained.log"} {
						if !strings.Contains(stdout.String(), want) {
							t.Fatalf("partial evidence missing %q: %s", want, stdout.String())
						}
					}
					return
				}
				var got report.Data
				if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Warnings) != 2 || got.Warnings[0] != deletedWarning || got.Warnings[1] != writerWarning || len(got.Files) != 1 || len(got.Files[0].Writers) != 1 || got.Files[0].Writers[0].PID != 123 || len(got.Deleted) != 1 || got.Deleted[0].AllocatedBytes != 8192 {
					t.Fatalf("partial evidence lost in JSON: %+v", got)
				}
				if format == "stream" && bytes.Count(stdout.Bytes(), []byte("\n")) != 1 {
					t.Fatalf("stream report must remain one NDJSON value: %s", stdout.String())
				}
			})
		}
	}
}

func TestRunReportsActualScanScopeForEachRoot(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	empty := filepath.Join(base, "empty")
	if err := os.MkdirAll(filepath.Join(data, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "child", "hidden"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(data, alias); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(data)
	if err != nil {
		t.Fatal(err)
	}
	oldAttribute, oldDeleted := attributeWriters, inspectDeletedFiles
	t.Cleanup(func() { attributeWriters, inspectDeletedFiles = oldAttribute, oldDeleted })
	attributeWriters = func([]disk.File) []string { return nil }
	inspectDeletedFiles = func() ([]proc.DeletedFile, []string) { return nil, nil }
	for _, mode := range []string{"standard", "light"} {
		for _, depth := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/depth%d", mode, depth), func(t *testing.T) {
				args := []string{alias, empty, "--depth", fmt.Sprint(depth), "--sample", "0"}
				if mode == "light" {
					args = append([]string{"light"}, args...)
				}
				var stdout, stderr bytes.Buffer
				if code := run(append(append([]string{}, args...), "--json"), &stdout, &stderr); code != 0 {
					t.Fatalf("run failed %d: %s", code, stderr.String())
				}
				var reports []report.Data
				if err := json.Unmarshal(stdout.Bytes(), &reports); err != nil {
					t.Fatal(err)
				}
				if len(reports) != 2 || reports[0].ScanScope == nil || reports[1].ScanScope == nil {
					t.Fatalf("missing per-root scope: %s", stdout.String())
				}
				scope := reports[0].ScanScope
				if scope.Root != resolved || scope.MaxDepth != depth || scope.RegularFiles != depth || scope.SkippedDepthDirectories != 1-depth || scope.SkippedSymlinks != 0 || reports[1].ScanScope.RegularFiles != 0 {
					t.Fatalf("incorrect scope: %+v / %+v", scope, reports[1].ScanScope)
				}
				stdout.Reset()
				if code := run(args, &stdout, &stderr); code != 0 {
					t.Fatalf("text run failed %d: %s", code, stderr.String())
				}
				if strings.Count(stdout.String(), "Scan scope:") != 2 || !strings.Contains(stdout.String(), fmt.Sprintf("max directory depth %d", depth)) {
					t.Fatalf("missing per-root text scope: %s", stdout.String())
				}
				if depth == 0 && !strings.Contains(stdout.String(), "increase --depth") {
					t.Fatalf("no next investigation step: %s", stdout.String())
				}
			})
		}
	}
}
