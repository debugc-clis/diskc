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

func TestPrintDirectoryEstimatesAndJSON(t *testing.T) {
	report := Report{Runtime: "docker", Paths: []PathReport{{Path: "/docker", Directories: []disk.Directory{
		{Path: "/docker/estimated\n\x1b[31m", AllocatedBytes: 8192, ApparentBytes: 8192, Estimated: true},
		{Path: "/docker/exact", AllocatedBytes: 4096, ApparentBytes: 4096},
	}}}}
	var output bytes.Buffer
	if err := printReport(&output, report); err != nil {
		t.Fatal(err)
	}
	estimatedRow := `8.0 KB  /docker/estimated\n\x1b[31m  (estimated)` + "\n"
	exactRow := "4.0 KB  /docker/exact\n"
	if !strings.Contains(output.String(), estimatedRow) || !strings.Contains(output.String(), exactRow) || strings.Count(output.String(), "(estimated)") != 1 {
		t.Fatalf("directory estimate labels incorrect: %s", output.String())
	}
	if strings.Index(output.String(), estimatedRow) > strings.Index(output.String(), exactRow) || strings.ContainsAny(output.String(), "\x1b\r") {
		t.Fatalf("directory order or terminal escaping changed: %q", output.String())
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Paths []struct {
			Directories []map[string]json.RawMessage `json:"directories"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Paths) != 1 || len(decoded.Paths[0].Directories) != 2 {
		t.Fatalf("directory evidence missing from JSON: %s", encoded)
	}
	directories := decoded.Paths[0].Directories
	if string(directories[0]["estimated"]) != "true" {
		t.Fatalf("estimated directory lost its JSON flag: %s", encoded)
	}
	if _, exists := directories[1]["estimated"]; exists {
		t.Fatalf("exact directory unexpectedly has a JSON estimate flag: %s", encoded)
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

func TestPrintApparentSizesWithExistingEvidence(t *testing.T) {
	files := []disk.File{
		{Path: "/data/sparse\n\x1b[31m", AllocatedBytes: 8192, ApparentBytes: 1 << 30, Growth: 1024},
		{Path: "/data/small", AllocatedBytes: 4096, ApparentBytes: 1, GrowthStatus: "replaced"},
		{Path: "/data/equal", AllocatedBytes: 2048, ApparentBytes: 2048},
		{Path: "/data/zero", AllocatedBytes: 1024, GrowthStatus: "unavailable"},
	}
	directories := []disk.Directory{
		{Path: "/data/sparse-dir\n\x1b[31m", AllocatedBytes: 8192, ApparentBytes: 1 << 30, Estimated: true},
		{Path: "/data/small-dir", AllocatedBytes: 4096, ApparentBytes: 1},
		{Path: "/data/equal-dir", AllocatedBytes: 2048, ApparentBytes: 2048},
		{Path: "/data/zero-dir", AllocatedBytes: 1024},
	}
	report := Report{Runtime: "docker", Paths: []PathReport{{Path: "/data", Files: files, Directories: directories}}}
	var output bytes.Buffer
	if err := printReport(&output, report); err != nil {
		t.Fatal(err)
	}
	expectedRows := []string{
		`8.0 KB  /data/sparse\n\x1b[31m  [1.0 GB apparent]  ↑ 1.0 KB/s`,
		"4.0 KB  /data/small  [1 B apparent]  [growth unmeasured: file replaced; resample]",
		"2.0 KB  /data/equal",
		"1.0 KB  /data/zero  [growth unmeasured: file unavailable or no longer regular; last observed size]",
		`8.0 KB  /data/sparse-dir\n\x1b[31m  [1.0 GB apparent]  (estimated)`,
		"4.0 KB  /data/small-dir  [1 B apparent]",
		"2.0 KB  /data/equal-dir",
		"1.0 KB  /data/zero-dir",
	}
	previous := -1
	for _, row := range expectedRows {
		index := strings.Index(output.String(), row+"\n")
		if index < 0 {
			t.Fatalf("missing complete row %q in output: %s", row, output.String())
		}
		if index <= previous {
			t.Fatalf("row order changed: %s", output.String())
		}
		previous = index
	}
	if strings.Count(output.String(), " apparent]") != 4 || strings.ContainsAny(output.String(), "\x1b\r") {
		t.Fatalf("apparent-size omission or terminal escaping changed: %q", output.String())
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	gotFiles, gotDirectories := decoded.Paths[0].Files, decoded.Paths[0].Directories
	for index, file := range gotFiles {
		if file.Path != files[index].Path || file.AllocatedBytes != files[index].AllocatedBytes || file.ApparentBytes != files[index].ApparentBytes || file.Growth != files[index].Growth || file.GrowthStatus != files[index].GrowthStatus {
			t.Fatalf("file evidence changed in JSON: %+v; want %+v", file, files[index])
		}
	}
	for index, directory := range gotDirectories {
		if directory != directories[index] {
			t.Fatalf("directory evidence changed in JSON: %+v; want %+v", directory, directories[index])
		}
	}
}

func TestSparseFileScanPreservesAllocatedRankingAndApparentSize(t *testing.T) {
	root := t.TempDir()
	sparsePath := filepath.Join(root, "sparse.img")
	sparse, err := os.Create(sparsePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(1 << 30); err != nil {
		_ = sparse.Close()
		t.Fatal(err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "allocated.bin"), bytes.Repeat([]byte{1}, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	oldDiscover, oldAttribute := discover, attributeWriters
	t.Cleanup(func() { discover, attributeWriters = oldDiscover, oldAttribute })
	discover = func() Report {
		return Report{Runtime: "docker", Paths: []PathReport{{Path: root, Kind: "test storage"}}}
	}
	attributeWriters = func([]disk.File) []string { return nil }
	var encoded bytes.Buffer
	if err := Run([]string{"--sample", "0", "--top", "2", "--json"}, &encoded, io.Discard); err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(encoded.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Paths) != 1 || report.Paths[0].Error != "" {
		t.Fatalf("unexpected report: %+v", report)
	}
	files := report.Paths[0].Files
	if len(files) != 2 || files[0].Path != filepath.Join(resolvedRoot, "allocated.bin") || files[1].Path != filepath.Join(resolvedRoot, "sparse.img") {
		t.Fatalf("files not ranked by allocated bytes: %+v", files)
	}
	if files[1].ApparentBytes != 1<<30 || files[1].AllocatedBytes >= files[0].AllocatedBytes || files[1].AllocatedBytes >= files[1].ApparentBytes {
		t.Fatalf("real sparse-file size evidence lost: %+v", files)
	}
	var output bytes.Buffer
	if err := Run([]string{"--sample", "0", "--top", "2"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	sparseRow := formatBytes(uint64(files[1].AllocatedBytes)) + "  " + filepath.Join(resolvedRoot, "sparse.img") + "  [1.0 GB apparent]"
	if !strings.Contains(output.String(), sparseRow+"\n") || strings.Index(output.String(), filepath.Join(resolvedRoot, "allocated.bin")) > strings.Index(output.String(), sparseRow) {
		t.Fatalf("sparse-file annotation or allocated order missing: %s", output.String())
	}
}
