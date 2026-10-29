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
	scan = func(path string, _ uint64, _, _ int) ([]disk.File, []disk.Directory, []string, error) {
		return []disk.File{
			{Path: path + "/large", AllocatedBytes: 300},
			{Path: path + "/medium", AllocatedBytes: 200},
			{Path: path + "/fast", AllocatedBytes: 10, Growth: 500},
		}, nil, []string{"partial scan"}, nil
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
	attributeWriters = func([]disk.File) { attributeCalls++ }
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
