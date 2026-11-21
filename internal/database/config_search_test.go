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

func TestConfigurationMatchesNestedPathsAndSymlinkDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"15/main", "16/main"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "postgresql.conf"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "ordinary-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "15"), filepath.Join(root, "17-link")); err != nil {
		t.Fatal(err)
	}
	pattern := filepath.Join(root, "*", "*", "postgresql.conf")
	want := []string{filepath.Join(root, "15/main/postgresql.conf"), filepath.Join(root, "16/main/postgresql.conf"), filepath.Join(root, "17-link/main/postgresql.conf")}
	got, warnings := configurationMatches([]string{pattern, want[0], filepath.Join(root, "missing/*.conf"), "relative/*.conf"}, os.Stat, os.ReadDir)
	if !reflect.DeepEqual(got, want) || len(warnings) != 0 {
		t.Fatalf("nested/config ordering mismatch: got=%v warnings=%v", got, warnings)
	}
}

func TestConfigurationMatchesFailuresAndPartialDirectoryContinue(t *testing.T) {
	root := t.TempDir()
	private, partial := filepath.Join(root, "private"), filepath.Join(root, "partial")
	for _, dir := range []string{private, partial} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a.conf", "b.conf", "c.conf"} {
		if err := os.WriteFile(filepath.Join(partial, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(partial)
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(partial, "b.conf")
	statCalls, dirCalls := make(map[string]int), make(map[string]int)
	stat := func(path string) (os.FileInfo, error) {
		statCalls[path]++
		if path == bad {
			return nil, &os.PathError{Op: "stat", Path: path, Err: syscall.EIO}
		}
		return os.Stat(path)
	}
	readDir := func(path string) ([]os.DirEntry, error) {
		dirCalls[path]++
		if path == private {
			return nil, &os.PathError{Op: "readdir", Path: path, Err: os.ErrPermission}
		}
		if path == partial {
			return []os.DirEntry{entries[2], entries[1], entries[0]}, &os.PathError{Op: "readdir", Path: path, Err: syscall.EIO}
		}
		return os.ReadDir(path)
	}
	got, warnings := configurationMatches([]string{filepath.Join(private, "*.conf"), filepath.Join(partial, "*.conf"), bad, filepath.Join(private, "*.cnf"), filepath.Join(partial, "*.conf")}, stat, readDir)
	want := []string{filepath.Join(partial, "a.conf"), filepath.Join(partial, "c.conf")}
	if !reflect.DeepEqual(got, want) || len(warnings) != 3 {
		t.Fatalf("lost usable partial entries or failures: got=%v warnings=%v", got, warnings)
	}
	for index, test := range []struct{ role, path, reason string }{{"configuration search directory", private, os.ErrPermission.Error()}, {"configuration search directory", partial, syscall.EIO.Error()}, {"configuration candidate", bad, syscall.EIO.Error()}} {
		for _, part := range []string{test.role, test.path, test.reason, "discovery may be incomplete", "verify the configuration search path and access", "affected service user"} {
			if !strings.Contains(warnings[index], part) {
				t.Fatalf("missing %q: %q", part, warnings[index])
			}
		}
	}
	if dirCalls[private] != 1 || dirCalls[partial] != 1 || statCalls[bad] != 1 {
		t.Fatalf("overlapping search reprobed failures: stat=%v readdir=%v", statCalls, dirCalls)
	}
	// A failed search must not prevent confirmed configs from being parsed in
	// pattern order, including the usable entries from a partial directory.
	var parsed []string
	instances := discoverInstancesWithSearch([]definition{{Engine: "redis"}}, nil, func(path, engine string) (map[string]string, parsedPaths, error) {
		parsed = append(parsed, path)
		return map[string]string{"appendonly": filepath.Base(path)}, parsedPaths{}, nil
	}, os.Stat, func([]string) ([]string, []string) { return got, warnings })
	if len(instances) != 1 || !reflect.DeepEqual(parsed, want) || !reflect.DeepEqual(instances[0].Configs, want) || instances[0].Settings["appendonly"] != "c.conf" || !reflect.DeepEqual(instances[0].Warnings, warnings) {
		t.Fatalf("search failed to preserve later configs: parsed=%v instances=%+v", parsed, instances)
	}
}

func TestConfigurationMatchesRealMetadataFailure(t *testing.T) {
	root := t.TempDir()
	loop, healthy := filepath.Join(root, "a.conf"), filepath.Join(root, "b.conf")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(healthy, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, warnings := configurationMatches([]string{loop, filepath.Join(root, "*.conf")}, os.Stat, os.ReadDir)
	if !reflect.DeepEqual(got, []string{healthy}) || len(warnings) != 1 || !strings.Contains(warnings[0], loop) || !strings.Contains(warnings[0], "configuration candidate") {
		t.Fatalf("real metadata failure lost: configs=%v warnings=%v", got, warnings)
	}
	// A looping search-directory link is a visibility failure too.
	got, warnings = configurationMatches([]string{filepath.Join(loop, "*.conf"), healthy}, os.Stat, os.ReadDir)
	if !reflect.DeepEqual(got, []string{healthy}) || len(warnings) != 1 || !strings.Contains(warnings[0], "configuration search directory") {
		t.Fatalf("real search directory failure lost: configs=%v warnings=%v", got, warnings)
	}
}

func TestConfigurationSearchWarningOnlyRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private\n\x1b[31m\r")
	instances := discoverInstancesWithSearch([]definition{{Engine: "redis", ConfigGlobs: []string{path}}}, nil, func(string, string) (map[string]string, parsedPaths, error) {
		t.Fatal("unconfirmed config must not be parsed")
		return nil, parsedPaths{}, nil
	}, os.Stat, func(patterns []string) ([]string, []string) {
		return configurationMatches(patterns, func(candidate string) (os.FileInfo, error) {
			return nil, &os.PathError{Op: "stat", Path: candidate, Err: os.ErrPermission}
		}, os.ReadDir)
	})
	if len(instances) != 1 || len(instances[0].Configs) != 0 || len(instances[0].Warnings) != 1 {
		t.Fatalf("warning-only candidate lost: %+v", instances)
	}
	oldDiscover, oldUsage, oldScan := discover, filesystemUsage, scan
	t.Cleanup(func() { discover, filesystemUsage, scan = oldDiscover, oldUsage, oldScan })
	discover = func() []Instance { return instances }
	filesystemUsage = func(string) (disk.Filesystem, error) {
		t.Fatal("unconfirmed config must not request filesystem usage")
		return disk.Filesystem{}, nil
	}
	scan = func(string, uint64, int, int) ([]disk.File, []disk.Directory, disk.ScanScope, []string, error) {
		t.Fatal("unconfirmed config must not scan")
		return nil, nil, disk.ScanScope{}, nil, nil
	}
	var output, stderr bytes.Buffer
	if err := Run([]string{"--sample", "0"}, &output, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"Database candidate: redis", "Discovery incomplete: no configuration, data directory, or process confirmed.", `private\n\x1b[31m\r`, "Warning:"} {
		if !strings.Contains(output.String(), part) {
			t.Fatalf("missing %q in %q", part, output.String())
		}
	}
	if strings.ContainsAny(output.String(), "\x1b\r") || strings.Contains(output.String(), "Filesystem:") || strings.Contains(output.String(), "No supported database") {
		t.Fatalf("unsafe/misleading text: %q", output.String())
	}
	output.Reset()
	if err := Run([]string{"--sample", "0", "--json"}, &output, &stderr); err != nil {
		t.Fatal(err)
	}
	var decoded []Instance
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || !reflect.DeepEqual(decoded[0].Warnings, instances[0].Warnings) {
		t.Fatalf("JSON warning fidelity lost: %s", output.String())
	}
	for _, forbidden := range []string{`"configs"`, `"filesystems"`, `"usage"`, `"data_paths"`, `"scan_scope"`, `"files"`, `"fastest_growing"`} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("invented evidence %s: %s", forbidden, output.String())
		}
	}
}
