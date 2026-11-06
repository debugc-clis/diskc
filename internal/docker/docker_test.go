package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debugc-clis/diskc/internal/disk"
)

func TestReadConfigDataRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "daemon.json")
	if err := os.WriteFile(path, []byte(`{"data-root":"/srv/docker"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	config, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.DataRoot != "/srv/docker" {
		t.Fatalf("unexpected data root: %q", config.DataRoot)
	}
}

func TestUniquePaths(t *testing.T) {
	paths := uniquePaths([]PathReport{{Path: "/var/lib/docker", Kind: "data"}, {Path: "/var/lib/docker", Kind: "duplicate"}})
	if len(paths) != 1 || paths[0].Kind != "data" {
		t.Fatalf("unexpected paths: %#v", paths)
	}
}

func TestRunHonorsTopAndUsesSharedSampleWindow(t *testing.T) {
	oldDiscover, oldUsage, oldScan := discover, filesystemUsage, scan
	oldMeasure, oldAttribute := measureGrowthGroups, attributeWriters
	t.Cleanup(func() {
		discover, filesystemUsage, scan = oldDiscover, oldUsage, oldScan
		measureGrowthGroups, attributeWriters = oldMeasure, oldAttribute
	})
	discover = func() Report {
		return Report{Runtime: "docker", Paths: []PathReport{{Path: "/docker"}, {Path: "/containerd"}}}
	}
	filesystemUsage = func(path string) (disk.Filesystem, error) {
		return disk.Filesystem{Path: path, Device: 1, AvailableBytes: 1000}, nil
	}
	scan = func(path string, _ uint64, _, _ int) ([]disk.File, []disk.Directory, disk.ScanScope, []string, error) {
		return []disk.File{
			{Path: path + "/large", AllocatedBytes: 300},
			{Path: path + "/medium", AllocatedBytes: 200},
			{Path: path + "/fast", AllocatedBytes: 10, Growth: 500},
		}, nil, disk.ScanScope{Root: path, MaxDepth: 4, RegularFiles: 3}, []string{"partial scan"}, nil
	}
	measureCalls := 0
	measureGrowthGroups = func(groups [][]disk.File, filesystems []*disk.Filesystem, interval time.Duration) error {
		measureCalls++
		if len(groups) != 2 || interval != time.Second {
			t.Fatalf("unexpected sample: groups=%d interval=%s", len(groups), interval)
		}
		return nil
	}
	attributeCalls := 0
	attributeWriters = func([]disk.File) []string { attributeCalls++; return nil }
	var stdout bytes.Buffer
	if err := Run([]string{"--top", "1", "--sample", "1s", "--json"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result Report
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if measureCalls != 1 || attributeCalls != 1 {
		t.Fatalf("expected one shared pass, got measure=%d attribute=%d", measureCalls, attributeCalls)
	}
	for _, path := range result.Paths {
		if len(path.Files) != 1 || path.Files[0].AllocatedBytes != 300 || len(path.Growing) != 1 || len(path.Warnings) != 1 {
			t.Fatalf("top or warnings not applied: %+v", path)
		}
	}
}

func TestRunKeepsUnavailablePath(t *testing.T) {
	oldDiscover, oldUsage := discover, filesystemUsage
	t.Cleanup(func() { discover, filesystemUsage = oldDiscover, oldUsage })
	discover = func() Report { return Report{Runtime: "docker", Paths: []PathReport{{Path: "/docker", Kind: "data"}}} }
	filesystemUsage = func(string) (disk.Filesystem, error) { return disk.Filesystem{}, errors.New("denied") }
	var stdout bytes.Buffer
	if err := Run([]string{"--sample", "0"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("Unavailable: cannot inspect: denied")) {
		t.Fatalf("unavailable path was hidden: %s", stdout.String())
	}
}

func TestPrintPreservesUnmeasuredGrowth(t *testing.T) {
	report := Report{Runtime: "docker", Paths: []PathReport{{Path: "/docker", Files: []disk.File{
		{Path: "/docker/replaced.log", AllocatedBytes: 4096, GrowthStatus: "replaced"},
		{Path: "/docker/missing.log", AllocatedBytes: 8192, GrowthStatus: "unavailable"},
	}}}}
	var output bytes.Buffer
	if err := printReport(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"file replaced; resample", "file unavailable or no longer regular; last observed size"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q in output: %s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), "Fastest growth") || strings.Contains(output.String(), "↑") {
		t.Fatalf("unmeasured growth presented as growing: %s", output.String())
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var got Report
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if files := got.Paths[0].Files; files[0].GrowthStatus != "replaced" || files[1].GrowthStatus != "unavailable" {
		t.Fatalf("growth status lost in JSON: %+v", files)
	}
}

func TestRunKeepsWritersWithTheirObservedRows(t *testing.T) {
	oldUsage, oldScan := filesystemUsage, scan
	oldMeasure, oldAttribute := measureGrowthGroups, attributeWriters
	t.Cleanup(func() {
		filesystemUsage, scan = oldUsage, oldScan
		measureGrowthGroups, attributeWriters = oldMeasure, oldAttribute
	})
	oldDiscover := discover
	t.Cleanup(func() { discover = oldDiscover })
	discover = func() Report { return Report{Paths: []PathReport{{Path: "/docker"}, {Path: "/docker/nested"}}} }
	filesystemUsage = func(path string) (disk.Filesystem, error) { return disk.Filesystem{Path: path, Device: 1}, nil }
	// Overlapping roots can observe different identities at the same pathname.
	scan = func(string, uint64, int, int) ([]disk.File, []disk.Directory, disk.ScanScope, []string, error) {
		return []disk.File{{Path: "/shared/app.log", AllocatedBytes: 100, Growth: 10}}, nil, disk.ScanScope{}, nil, nil
	}
	measureGrowthGroups = func([][]disk.File, []*disk.Filesystem, time.Duration) error { return nil }
	calls := 0
	attributeWriters = func(files []disk.File) []string {
		calls++
		if len(files) != 4 {
			t.Fatalf("got %d attribution rows, want 4", len(files))
		}
		for index := range files {
			if index != 2 {
				files[index].Writers = []disk.Writer{{PID: index + 1}}
			}
		}
		return nil
	}
	var stdout bytes.Buffer
	if err := Run([]string{"--top", "1", "--sample", "0", "--json"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result Report
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("got %d attribution calls, want 1", calls)
	}
	rows := []disk.File{result.Paths[0].Files[0], result.Paths[0].Growing[0], result.Paths[1].Files[0], result.Paths[1].Growing[0]}
	for index, row := range rows {
		if index == 2 {
			if len(row.Writers) != 0 {
				t.Fatalf("empty writer result inherited another row: %+v", row.Writers)
			}
		} else if len(row.Writers) != 1 || row.Writers[0].PID != index+1 {
			t.Fatalf("row %d got writers %+v, want PID %d", index, row.Writers, index+1)
		}
	}
}

func TestRunPropagatesWriterWarningsOnlyToInspectedPaths(t *testing.T) {
	oldDiscover, oldUsage, oldScan := discover, filesystemUsage, scan
	oldMeasure, oldAttribute := measureGrowthGroups, attributeWriters
	t.Cleanup(func() {
		discover, filesystemUsage, scan = oldDiscover, oldUsage, oldScan
		measureGrowthGroups, attributeWriters = oldMeasure, oldAttribute
	})
	discover = func() Report {
		return Report{Runtime: "docker", Paths: []PathReport{{Path: "/docker"}, {Path: "/unavailable"}, {Path: "/scan-failed"}}}
	}
	filesystemUsage = func(path string) (disk.Filesystem, error) {
		if path == "/unavailable" {
			return disk.Filesystem{}, errors.New("denied")
		}
		return disk.Filesystem{Path: path, Device: 1}, nil
	}
	scan = func(path string, _ uint64, _, _ int) ([]disk.File, []disk.Directory, disk.ScanScope, []string, error) {
		if path == "/scan-failed" {
			return nil, nil, disk.ScanScope{}, nil, errors.New("cannot scan")
		}
		return []disk.File{{Path: path + "/app.log", AllocatedBytes: 100}}, nil, disk.ScanScope{Root: path, RegularFiles: 1}, []string{"scan warning"}, nil
	}
	measureGrowthGroups = func([][]disk.File, []*disk.Filesystem, time.Duration) error { return nil }
	const warning = "writer attribution partial; no visible match does not establish absence"
	attributeWriters = func(files []disk.File) []string {
		if len(files) != 1 {
			t.Fatalf("expected only successful scan rows, got %+v", files)
		}
		files[0].Writers = []disk.Writer{{PID: 123}}
		return []string{warning}
	}
	for _, jsonOutput := range []bool{false, true} {
		var stdout bytes.Buffer
		args := []string{"--sample", "0"}
		if jsonOutput {
			args = append(args, "--json")
		}
		if err := Run(args, &stdout, io.Discard); err != nil {
			t.Fatal(err)
		}
		if strings.Count(stdout.String(), warning) != 1 {
			t.Fatalf("expected warning only on successful path: %s", stdout.String())
		}
		if !jsonOutput {
			continue
		}
		var got Report
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		for index, path := range got.Paths {
			if index == 0 {
				if len(path.Warnings) != 2 || path.Warnings[0] != "scan warning" || path.Warnings[1] != warning || len(path.Files[0].Writers) != 1 || path.Files[0].Writers[0].PID != 123 {
					t.Fatalf("successful partial evidence lost: %+v", path)
				}
			} else if path.Error == "" || len(path.Warnings) != 0 {
				t.Fatalf("process warning attached to failed path: %+v", path)
			}
		}
	}
}

func TestRunEmptyScopeAndFailedPathsInTextAndJSON(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child", "data"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDiscover, oldAttribute := discover, attributeWriters
	t.Cleanup(func() { discover, attributeWriters = oldDiscover, oldAttribute })
	discover = func() Report {
		return Report{Runtime: "docker", Paths: []PathReport{{Path: root}, {Path: filepath.Join(root, "missing")}}}
	}
	attributeWriters = func([]disk.File) []string { return nil }
	var output bytes.Buffer
	if err := Run([]string{"--depth", "0", "--sample", "0", "--json"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	paths := decoded.Paths
	if len(paths) != 2 || paths[0].ScanScope == nil || paths[0].ScanScope.RegularFiles != 0 || paths[0].ScanScope.SkippedDepthDirectories != 1 || paths[0].ScanScope.MaxDepth != 0 || paths[1].ScanScope != nil || paths[1].Error == "" || strings.Count(output.String(), `"scan_scope"`) != 1 {
		t.Fatalf("incorrect scan scopes: %s", output.String())
	}
	output.Reset()
	if err := Run([]string{"--depth", "0", "--sample", "0"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "Scan scope:") != 1 || !strings.Contains(output.String(), "max directory depth 0") || !strings.Contains(output.String(), "increase --depth") {
		t.Fatalf("empty scope lost in text: %s", output.String())
	}
}
