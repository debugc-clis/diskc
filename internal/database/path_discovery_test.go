package database

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/debugc-clis/diskc/internal/disk"
)

func TestDiscoverCandidateMetadataFailuresContinue(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "redis.conf")
	if err := os.WriteFile(config, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	badData, badLog := filepath.Join(root, "private-data"), filepath.Join(root, "broken-log")
	goodData, goodLog := filepath.Join(root, "data"), filepath.Join(root, "redis.log")
	missing := filepath.Join(root, "missing")
	calls := make(map[string]int)
	stat := func(path string) (os.FileInfo, error) {
		calls[path]++
		switch path {
		case badData:
			return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrPermission}
		case badLog:
			return nil, &os.PathError{Op: "stat", Path: path, Err: syscall.EIO}
		case missing:
			return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
		case goodData, goodLog:
			return nil, nil
		default:
			t.Fatalf("unexpected stat of %q", path)
			return nil, nil
		}
	}
	defs := []definition{{Engine: "redis", ConfigGlobs: []string{config}, DataPaths: []string{badData, missing, badData, "relative", goodData}}}
	instances := discoverInstancesWithStat(defs, nil, func(path, engine string) (map[string]string, parsedPaths, error) {
		if path != config || engine != "redis" {
			t.Fatalf("unexpected config %s/%s", engine, path)
		}
		return map[string]string{"appendonly": "yes"}, parsedPaths{data: []string{badData, goodData, missing}, logs: []string{badLog, goodLog, badLog, missing, "relative"}}, nil
	}, stat)
	if len(instances) != 1 {
		t.Fatalf("expected instance: %+v", instances)
	}
	instance := instances[0]
	if !reflect.DeepEqual(instance.DataPaths, []string{goodData}) || !reflect.DeepEqual(instance.LogPaths, []string{goodLog}) || instance.Settings["appendonly"] != "yes" {
		t.Fatalf("lost confirmed observations: %+v", instance)
	}
	if len(instance.Warnings) != 2 {
		t.Fatalf("expected data and log warnings: %+v", instance)
	}
	for index, want := range []struct{ role, path, reason string }{{"data", badData, os.ErrPermission.Error()}, {"log", badLog, syscall.EIO.Error()}} {
		for _, part := range []string{"cannot inspect " + want.role + " path", want.path, want.reason, "discovery may be incomplete", "verify the path and access", "affected service user"} {
			if !strings.Contains(instance.Warnings[index], part) {
				t.Fatalf("missing %q in %q", part, instance.Warnings[index])
			}
		}
	}
	for _, path := range []string{badData, badLog, goodData, goodLog} {
		if calls[path] != 1 {
			t.Fatalf("candidate %s probed %d times", path, calls[path])
		}
	}
	if calls[missing] != 2 { // One deduplicated probe for each candidate role.
		t.Fatalf("missing candidate probes = %d", calls[missing])
	}
}

func TestDiscoverCandidateOptionalAbsenceQuiet(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	defs := []definition{{Engine: "redis", DataPaths: []string{missing, missing, "relative"}}}
	calls := 0
	stat := func(path string) (os.FileInfo, error) {
		calls++
		if path != missing {
			t.Fatalf("unexpected probe: %q", path)
		}
		return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
	}
	instances := discoverInstancesWithStat(defs, nil, parseConfig, stat)
	if len(instances) != 0 || calls != 1 {
		t.Fatalf("optional absence invented evidence or retried: %+v calls=%d", instances, calls)
	}
	instances = discoverInstancesWithStat(defs, map[string][]Process{"redis": {{PID: 42, Process: "redis-server"}}}, parseConfig, stat)
	if len(instances) != 1 || len(instances[0].Warnings) != 0 || len(instances[0].DataPaths) != 0 {
		t.Fatalf("missing optional path warned: %+v", instances)
	}
}

func TestDiscoverCandidateFailureOnlyReachesRunWithoutMeasurements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data\n\x1b[31m\r")
	instances := discoverInstancesWithStat([]definition{{Engine: "redis", DataPaths: []string{path, path}}}, nil, parseConfig, func(candidate string) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "stat", Path: candidate, Err: syscall.EIO}
	})
	if len(instances) != 1 || len(instances[0].Warnings) != 1 || len(instances[0].DataPaths) != 0 || len(instances[0].Configs) != 0 || len(instances[0].Filesystems) != 0 {
		t.Fatalf("warning-only candidate lost or confirmed: %+v", instances)
	}
	oldDiscover, oldUsage, oldScan := discover, filesystemUsage, scan
	t.Cleanup(func() { discover, filesystemUsage, scan = oldDiscover, oldUsage, oldScan })
	discover = func() []Instance { return instances }
	filesystemUsage = func(string) (disk.Filesystem, error) {
		t.Fatal("unconfirmed candidate must not request filesystem usage")
		return disk.Filesystem{}, nil
	}
	scan = func(string, uint64, int, int) ([]disk.File, []disk.Directory, disk.ScanScope, []string, error) {
		t.Fatal("unconfirmed candidate must not scan")
		return nil, nil, disk.ScanScope{}, nil, nil
	}
	var output, stderr bytes.Buffer
	if err := Run([]string{"--sample", "0"}, &output, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"Database candidate: redis", "Discovery incomplete: no configuration, data directory, or process confirmed.", "Warning:", `data\n\x1b[31m\r`} {
		if !strings.Contains(output.String(), part) {
			t.Fatalf("missing %q in %q", part, output.String())
		}
	}
	for _, forbidden := range []string{"No supported database", "Filesystem:", "Pressure:", "Largest", "\x1b", "\r"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("unexpected %q in %q", forbidden, output.String())
		}
	}
	output.Reset()
	if err := Run([]string{"--sample", "0", "--json"}, &output, &stderr); err != nil {
		t.Fatal(err)
	}
	var decoded []Instance
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].Engine != "redis" || !reflect.DeepEqual(decoded[0].Warnings, instances[0].Warnings) {
		t.Fatalf("JSON candidate/warning fidelity lost: %s", output.String())
	}
	for _, forbidden := range []string{`"filesystems"`, `"usage"`, `"data_paths"`, `"scan_scope"`, `"files"`, `"fastest_growing"`} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("invented evidence %s: %s", forbidden, output.String())
		}
	}
}

func TestDiscoverCandidateRealStatFailureAndContinuation(t *testing.T) {
	root := t.TempDir()
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	instances := discoverInstances([]definition{{Engine: "redis", DataPaths: []string{loop, data}}}, nil, parseConfig)
	if len(instances) != 1 || len(instances[0].Warnings) != 1 || !strings.Contains(instances[0].Warnings[0], loop) || !reflect.DeepEqual(instances[0].DataPaths, []string{data}) {
		t.Fatalf("real metadata failure lost or blocked continuation: %+v", instances)
	}
}
