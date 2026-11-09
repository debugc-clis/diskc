package database

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debugc-clis/diskc/internal/disk"
)

func TestParsePostgreSQLConfig(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "postgresql.conf")
	content := "data_directory = '/srv/postgres/data'\nport = 5432\narchive_mode = on\nshared_buffers = 1GB\n"
	if err := os.WriteFile(config, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, paths := parseConfig(config, "postgresql")
	if settings["port"] != "5432" || settings["archive_mode"] != "on" || settings["shared_buffers"] != "1GB" {
		t.Fatalf("unexpected settings: %#v", settings)
	}
	if len(paths.data) != 1 || paths.data[0] != "/srv/postgres/data" {
		t.Fatalf("unexpected data paths: %#v", paths.data)
	}
}

func TestParseRedisRelativePaths(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "redis.conf")
	if err := os.WriteFile(config, []byte("dir ./data\nappendonly yes\nappendfilename appendonly.aof\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, paths := parseConfig(config, "redis")
	if settings["appendonly"] != "yes" || settings["appendfilename"] != "appendonly.aof" {
		t.Fatalf("unexpected settings: %#v", settings)
	}
	if len(paths.data) != 1 || paths.data[0] != filepath.Join(root, "./data") {
		t.Fatalf("unexpected data paths: %#v", paths.data)
	}
}

func TestInspectHonorsTopAndUsesSharedSampleWindow(t *testing.T) {
	oldUsage, oldScan := filesystemUsage, scan
	oldMeasure, oldAttribute := measureGrowthGroups, attributeWriters
	t.Cleanup(func() {
		filesystemUsage, scan = oldUsage, oldScan
		measureGrowthGroups, attributeWriters = oldMeasure, oldAttribute
	})
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
			t.Fatalf("unexpected grouped sample: groups=%d interval=%s", len(groups), interval)
		}
		return nil
	}
	attributeCalls := 0
	attributeWriters = func([]disk.File) []string { attributeCalls++; return nil }
	instances := []Instance{{Engine: "postgresql", DataPaths: []string{"/db/one", "/db/two"}}}
	inspect(instances, 1, 4, time.Second)
	if measureCalls != 1 || attributeCalls != 1 {
		t.Fatalf("expected one shared sample and proc pass, got measure=%d attribute=%d", measureCalls, attributeCalls)
	}
	for _, filesystem := range instances[0].Filesystems {
		if len(filesystem.Files) != 1 || filesystem.Files[0].AllocatedBytes != 300 || len(filesystem.Growing) != 1 {
			t.Fatalf("top limit not applied: %+v", filesystem)
		}
		if len(filesystem.Warnings) != 1 {
			t.Fatalf("scan warnings not surfaced: %+v", filesystem.Warnings)
		}
	}
}

func TestInspectSurfacesUnavailableFilesystem(t *testing.T) {
	oldUsage := filesystemUsage
	t.Cleanup(func() { filesystemUsage = oldUsage })
	filesystemUsage = func(string) (disk.Filesystem, error) { return disk.Filesystem{}, errors.New("denied") }
	instances := []Instance{{Engine: "mysql", DataPaths: []string{"/db"}}}
	inspect(instances, 1, 1, 0)
	if len(instances[0].Filesystems) != 1 || !strings.Contains(instances[0].Filesystems[0].Error, "denied") {
		t.Fatalf("unavailable filesystem was dropped: %+v", instances)
	}
	var output bytes.Buffer
	if err := Print(&output, instances, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Unavailable") {
		t.Fatalf("missing unavailable state: %s", output.String())
	}
}

func TestPrintPreservesUnmeasuredGrowth(t *testing.T) {
	instances := []Instance{{Engine: "postgresql", Filesystems: []Filesystem{{Path: "/db", Files: []disk.File{
		{Path: "/db/replaced.log", AllocatedBytes: 4096, GrowthStatus: "replaced"},
		{Path: "/db/missing.log", AllocatedBytes: 8192, GrowthStatus: "unavailable"},
	}}}}}
	var output bytes.Buffer
	if err := Print(&output, instances, false); err != nil {
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
	output.Reset()
	if err := Print(&output, instances, true); err != nil {
		t.Fatal(err)
	}
	var got []Instance
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if files := got[0].Filesystems[0].Files; files[0].GrowthStatus != "replaced" || files[1].GrowthStatus != "unavailable" {
		t.Fatalf("growth status lost in JSON: %+v", files)
	}
}

func TestPrintDistinguishesAllocatedAndApparentSizes(t *testing.T) {
	files := []disk.File{
		{Path: "/db/sparse\n\x1b[31m", AllocatedBytes: 8192, ApparentBytes: 1 << 30, Growth: 1024, GrowthStatus: "measured"},
		{Path: "/db/small", AllocatedBytes: 4096, ApparentBytes: 5, GrowthStatus: "unavailable"},
		{Path: "/db/equal", AllocatedBytes: 2048, ApparentBytes: 2048},
		{Path: "/db/empty", AllocatedBytes: 0, ApparentBytes: 0},
	}
	instances := []Instance{{Engine: "postgresql", Filesystems: []Filesystem{{Path: "/db", Files: files}}}}
	var output bytes.Buffer
	if err := Print(&output, instances, false); err != nil {
		t.Fatal(err)
	}
	rows := []string{
		`8.0 KB  /db/sparse\n\x1b[31m  [1.0 GB apparent]  ↑ 1.0 KB/s` + "\n",
		"4.0 KB  /db/small  [5 B apparent]  [growth unmeasured: file unavailable or no longer regular; last observed size]\n",
		"2.0 KB  /db/equal\n",
		"0 B  /db/empty\n",
	}
	previous := -1
	for _, row := range rows {
		index := strings.Index(output.String(), row)
		if index < 0 || index <= previous {
			t.Fatalf("missing or reordered row %q: %s", row, output.String())
		}
		previous = index
	}
	if strings.Count(output.String(), " apparent]") != 2 || strings.ContainsAny(output.String(), "\x1b\r") {
		t.Fatalf("incorrect annotations or unescaped terminal controls: %q", output.String())
	}
	output.Reset()
	if err := Print(&output, instances, true); err != nil {
		t.Fatal(err)
	}
	var decoded []Instance
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	got := decoded[0].Filesystems[0].Files
	if len(got) != len(files) {
		t.Fatalf("JSON changed file count: %s", output.String())
	}
	for index, want := range files {
		if got[index].Path != want.Path || got[index].AllocatedBytes != want.AllocatedBytes || got[index].ApparentBytes != want.ApparentBytes || got[index].Growth != want.Growth || got[index].GrowthStatus != want.GrowthStatus {
			t.Fatalf("JSON changed observed file data: got %+v; want %+v", got[index], want)
		}
	}
}

func TestInspectKeepsWritersWithTheirObservedRows(t *testing.T) {
	oldUsage, oldScan := filesystemUsage, scan
	oldMeasure, oldAttribute := measureGrowthGroups, attributeWriters
	t.Cleanup(func() {
		filesystemUsage, scan = oldUsage, oldScan
		measureGrowthGroups, attributeWriters = oldMeasure, oldAttribute
	})

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
	instances := []Instance{{Engine: "postgresql", DataPaths: []string{"/db", "/db/nested"}}}
	inspect(instances, 1, 4, 0)
	if calls != 1 {
		t.Fatalf("got %d attribution calls, want 1", calls)
	}
	rows := []disk.File{instances[0].Filesystems[0].Files[0], instances[0].Filesystems[0].Growing[0], instances[0].Filesystems[1].Files[0], instances[0].Filesystems[1].Growing[0]}
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

func TestInspectPropagatesWriterWarningsOnlyToInspectedPaths(t *testing.T) {
	oldUsage, oldScan := filesystemUsage, scan
	oldMeasure, oldAttribute := measureGrowthGroups, attributeWriters
	t.Cleanup(func() {
		filesystemUsage, scan = oldUsage, oldScan
		measureGrowthGroups, attributeWriters = oldMeasure, oldAttribute
	})
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
	instances := []Instance{{Engine: "postgresql", DataPaths: []string{"/db", "/unavailable", "/scan-failed"}}}
	inspect(instances, 1, 4, 0)
	for index, filesystem := range instances[0].Filesystems {
		if index == 0 {
			if len(filesystem.Warnings) != 2 || filesystem.Warnings[0] != "scan warning" || filesystem.Warnings[1] != warning || len(filesystem.Files[0].Writers) != 1 || filesystem.Files[0].Writers[0].PID != 123 {
				t.Fatalf("successful partial evidence lost: %+v", filesystem)
			}
		} else if filesystem.Error == "" || len(filesystem.Warnings) != 0 {
			t.Fatalf("process warning attached to failed path: %+v", filesystem)
		}
	}
	for _, jsonOutput := range []bool{false, true} {
		var stdout bytes.Buffer
		if err := Print(&stdout, instances, jsonOutput); err != nil {
			t.Fatal(err)
		}
		if strings.Count(stdout.String(), warning) != 1 {
			t.Fatalf("expected warning only on successful path: %s", stdout.String())
		}
		if jsonOutput {
			var got []Instance
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got[0].Filesystems[0].Files[0].Writers) != 1 || got[0].Filesystems[0].Files[0].Writers[0].PID != 123 {
				t.Fatalf("visible writer lost from JSON: %+v", got)
			}
		}
	}
}

func TestInspectEmptyScopeAndFailedPathsInTextAndJSON(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child", "data"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	oldAttribute := attributeWriters
	t.Cleanup(func() { attributeWriters = oldAttribute })
	attributeWriters = func([]disk.File) []string { return nil }
	instances := []Instance{{Engine: "postgresql", DataPaths: []string{root, filepath.Join(root, "missing")}}}
	inspect(instances, 1, 0, 0)
	paths := instances[0].Filesystems
	if len(paths) != 2 || paths[0].ScanScope == nil || paths[0].ScanScope.RegularFiles != 0 || paths[0].ScanScope.SkippedDepthDirectories != 1 || paths[0].ScanScope.MaxDepth != 0 || paths[1].ScanScope != nil || paths[1].Error == "" {
		t.Fatalf("incorrect scan scopes: %+v", paths)
	}
	var output bytes.Buffer
	if err := Print(&output, instances, false); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "Scan scope:") != 1 || !strings.Contains(output.String(), "max directory depth 0") || !strings.Contains(output.String(), "increase --depth") {
		t.Fatalf("empty scope lost in text: %s", output.String())
	}
	output.Reset()
	if err := Print(&output, instances, true); err != nil {
		t.Fatal(err)
	}
	var decoded []Instance
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded[0].Filesystems[0].ScanScope == nil || *decoded[0].Filesystems[0].ScanScope != *paths[0].ScanScope || strings.Count(output.String(), `"scan_scope"`) != 1 {
		t.Fatalf("scope lost or failed scan claimed: %s", output.String())
	}
}
