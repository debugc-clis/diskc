package database

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

type failedConfigReader struct{ err error }

func (r failedConfigReader) Read([]byte) (int, error) { return 0, r.err }

func TestParseConfigReadFailuresKeepObservedPrefix(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "redis.conf")
	prefix := "dir ./data\nlogfile ./logs/redis.log\nappendonly yes\n"
	for _, test := range []struct {
		name   string
		reader io.Reader
		isErr  error
	}{
		{"io", io.MultiReader(strings.NewReader(prefix), failedConfigReader{syscall.EIO}), syscall.EIO},
		{"long_line", strings.NewReader(prefix + strings.Repeat("x", bufio.MaxScanTokenSize) + "\n"), bufio.ErrTooLong},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings, paths, err := parseConfigReader(test.reader, path, "redis")
			if !errors.Is(err, test.isErr) {
				t.Fatalf("expected %v, got %v", test.isErr, err)
			}
			if settings["appendonly"] != "yes" || !reflect.DeepEqual(paths.data, []string{filepath.Join(root, "data")}) || !reflect.DeepEqual(paths.logs, []string{filepath.Join(root, "logs/redis.log")}) {
				t.Fatalf("lost prefix: settings=%v paths=%+v", settings, paths)
			}
		})
	}
}

func TestParseConfigOpenFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.conf")
	settings, paths, err := parseConfig(path, "redis")
	if !errors.Is(err, os.ErrNotExist) || len(settings) != 0 || len(paths.data) != 0 || len(paths.logs) != 0 {
		t.Fatalf("open failure lost or invented observations: settings=%v paths=%+v err=%v", settings, paths, err)
	}
}

func TestDiscoverConfigFailuresContinueAndReachRun(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	logs := filepath.Join(root, "redis.log")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logs, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	failed := filepath.Join(root, "a\n\x1b[31m.conf")
	healthy := filepath.Join(root, "b.conf")
	for _, path := range []string{failed, healthy} {
		if err := os.WriteFile(path, []byte("dir ./data\nlogfile ./redis.log\nappendonly yes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defs := []definition{{Engine: "redis", ConfigGlobs: []string{filepath.Join(root, "*.conf")}}}
	for _, failure := range []error{os.ErrPermission, syscall.EIO, os.ErrNotExist} {
		t.Run(failure.Error(), func(t *testing.T) {
			instances := discoverInstances(defs, nil, func(path, engine string) (map[string]string, parsedPaths, error) {
				if path == failed {
					return nil, parsedPaths{}, &os.PathError{Op: "open", Path: path, Err: failure}
				}
				return parseConfig(path, engine)
			})
			if len(instances) != 1 {
				t.Fatalf("expected instance: %+v", instances)
			}
			instance := instances[0]
			if len(instance.Configs) != 2 || instance.Settings["appendonly"] != "yes" || !reflect.DeepEqual(instance.DataPaths, []string{data}) || !reflect.DeepEqual(instance.LogPaths, []string{logs}) {
				t.Fatalf("healthy config not preserved: %+v", instance)
			}
			if len(instance.Warnings) != 1 {
				t.Fatalf("expected one warning: %+v", instance)
			}
			warning := instance.Warnings[0]
			for _, want := range []string{failed, failure.Error(), "discovery may be incomplete", "verify the configuration and access", "affected service user"} {
				if !strings.Contains(warning, want) {
					t.Fatalf("missing %q in %q", want, warning)
				}
			}
			// Use Run's normal discovery hook, omitting data roots here so this
			// output check cannot depend on host scans or process visibility.
			oldDiscover := discover
			t.Cleanup(func() { discover = oldDiscover })
			discover = func() []Instance {
				return []Instance{{Engine: instance.Engine, Configs: instance.Configs, Warnings: instance.Warnings}}
			}
			var output, stderr bytes.Buffer
			if err := Run([]string{"--sample", "0"}, &output, &stderr); err != nil {
				t.Fatal(err)
			}
			if strings.ContainsAny(output.String(), "\x1b\r") || !strings.Contains(output.String(), `a\n\x1b[31m.conf`) || !strings.Contains(output.String(), "Warning: ") || strings.Contains(output.String(), "No supported database") {
				t.Fatalf("unsafe or missing text warning: %q", output.String())
			}
			output.Reset()
			if err := Run([]string{"--sample", "0", "--json"}, &output, &stderr); err != nil {
				t.Fatal(err)
			}
			var decoded []Instance
			if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded) != 1 || !reflect.DeepEqual(decoded[0].Warnings, instance.Warnings) || !reflect.DeepEqual(decoded[0].Configs, instance.Configs) {
				t.Fatalf("JSON warning/config fidelity lost: %s", output.String())
			}
		})
	}
}

func TestDiscoverRealConfigReadFailureAndPartialObservations(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "redis.conf")
	content := "dir ./data\nappendonly yes\n" + strings.Repeat("x", bufio.MaxScanTokenSize) + "\n"
	if err := os.WriteFile(config, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	instances := discoverInstances([]definition{{Engine: "redis", ConfigGlobs: []string{config}}}, nil, parseConfig)
	if len(instances) != 1 || len(instances[0].Warnings) != 1 || !strings.Contains(instances[0].Warnings[0], "token too long") || instances[0].Settings["appendonly"] != "yes" || !reflect.DeepEqual(instances[0].DataPaths, []string{data}) {
		t.Fatalf("lost partial configuration evidence: %+v", instances)
	}

	// A directory is stat-visible but cannot be read as a configuration on
	// supported platforms, exercising the real file-to-scanner error path.
	unreadable := filepath.Join(root, "directory.conf")
	if err := os.Mkdir(unreadable, 0o755); err != nil {
		t.Fatal(err)
	}
	instances = discoverInstances([]definition{{Engine: "redis", ConfigGlobs: []string{unreadable}}}, nil, parseConfig)
	if len(instances) != 1 || len(instances[0].Warnings) != 1 || len(instances[0].DataPaths) != 0 || len(instances[0].Filesystems) != 0 || !reflect.DeepEqual(instances[0].Configs, []string{unreadable}) {
		t.Fatalf("config-only failure omitted or invented storage: %+v", instances)
	}
}

func TestDiscoverMissingOptionalConfigsQuiet(t *testing.T) {
	root := t.TempDir()
	defs := []definition{{Engine: "redis", ConfigGlobs: []string{filepath.Join(root, "missing.conf"), filepath.Join(root, "*.cnf")}}}
	instances := discoverInstances(defs, nil, func(string, string) (map[string]string, parsedPaths, error) {
		t.Fatal("missing optional configuration should not be parsed")
		return nil, parsedPaths{}, nil
	})
	if len(instances) != 0 {
		t.Fatalf("invented instance for absent optional config: %+v", instances)
	}
	instances = discoverInstances(defs, map[string][]Process{"redis": {{PID: 123, Process: "redis-server"}}}, parseConfig)
	if len(instances) != 1 || len(instances[0].Warnings) != 0 {
		t.Fatalf("optional config absence warned for visible process: %+v", instances)
	}
	var output bytes.Buffer
	if err := Print(&output, instances, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `"warnings"`) {
		t.Fatalf("empty warnings must remain omitted: %s", output.String())
	}
}
