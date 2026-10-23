package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	scan = func(path string, _ uint64, _, _ int) ([]disk.File, []disk.Directory, []string, error) {
		return []disk.File{
			{Path: path + "/large", AllocatedBytes: 300},
			{Path: path + "/medium", AllocatedBytes: 200},
			{Path: path + "/fast", AllocatedBytes: 10, Growth: 500},
		}, nil, []string{"partial scan"}, nil
	}
	measureCalls := 0
	measureGrowthGroups = func(groups [][]disk.File, interval time.Duration) error {
		measureCalls++
		if len(groups) != 2 || interval != time.Second {
			t.Fatalf("unexpected sample: groups=%d interval=%s", len(groups), interval)
		}
		return nil
	}
	attributeCalls := 0
	attributeWriters = func([]disk.File) { attributeCalls++ }
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
