package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestDiscoverDataRootConfigFailures(t *testing.T) {
	configPaths := []string{"/etc/docker/daemon.json", "/home/operator/.config/docker/daemon.json"}
	fallbackPaths := []string{"/var/lib/docker", "/home/operator/.local/share/docker"}
	missing := &os.PathError{Op: "open", Path: configPaths[0], Err: os.ErrNotExist}
	permission := &os.PathError{Op: "open", Path: configPaths[0], Err: os.ErrPermission}
	ioFailure := &os.PathError{Op: "read", Path: configPaths[0], Err: syscall.EIO}
	for _, test := range []struct {
		name         string
		configs      []daemonConfig
		errors       []error
		fallback     string
		wantConfig   string
		wantRoot     string
		wantReads    int
		wantFallback bool
		wantWarning  error
	}{
		{
			name: "missing optional configs are quiet", errors: []error{missing, missing},
			wantReads: 2, wantFallback: true,
		},
		{
			name:    "permission failure then valid rootless config",
			configs: []daemonConfig{{}, {DataRoot: "/srv/rootless-docker"}}, errors: []error{permission, nil},
			wantConfig: configPaths[1], wantRoot: "/srv/rootless-docker", wantReads: 2, wantWarning: permission,
		},
		{
			name: "I/O failure with conventional fallback", errors: []error{ioFailure, missing},
			fallback: fallbackPaths[0], wantRoot: fallbackPaths[0], wantReads: 2, wantFallback: true, wantWarning: ioFailure,
		},
		{
			name: "permission failure without visible roots", errors: []error{permission, missing},
			wantReads: 2, wantFallback: true, wantWarning: permission,
		},
		{
			name:       "first successful config preserves precedence",
			configs:    []daemonConfig{{DataRoot: "/srv/rootful-docker"}, {DataRoot: "/srv/rootless-docker"}},
			wantConfig: configPaths[0], wantRoot: "/srv/rootful-docker", wantReads: 1,
		},
		{
			name:    "empty valid config stops search and uses fallback",
			configs: []daemonConfig{{}, {DataRoot: "/srv/rootless-docker"}}, fallback: fallbackPaths[0],
			wantConfig: configPaths[0], wantRoot: fallbackPaths[0], wantReads: 1, wantFallback: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			fallbackCalled := false
			var report Report
			discoverDataRoot(&report, configPaths, fallbackPaths,
				func(path string) (daemonConfig, error) {
					index := reads
					reads++
					if index >= len(configPaths) || path != configPaths[index] {
						t.Fatalf("unexpected config read %q at index %d", path, index)
					}
					var config daemonConfig
					var err error
					if index < len(test.configs) {
						config = test.configs[index]
					}
					if index < len(test.errors) {
						err = test.errors[index]
					}
					return config, err
				},
				func(paths []string) string {
					fallbackCalled = true
					if !reflect.DeepEqual(paths, fallbackPaths) {
						t.Fatalf("fallback paths changed: %v", paths)
					}
					return test.fallback
				})
			if reads != test.wantReads || fallbackCalled != test.wantFallback || report.Config != test.wantConfig || report.DataRoot != test.wantRoot {
				t.Fatalf("unexpected discovery: reads=%d fallback=%t report=%+v", reads, fallbackCalled, report)
			}
			if test.wantWarning == nil {
				if len(report.Warnings) != 0 {
					t.Fatalf("unexpected warnings: %v", report.Warnings)
				}
				return
			}
			if len(report.Warnings) != 1 {
				t.Fatalf("expected one config warning, got %v", report.Warnings)
			}
			for _, expected := range []string{configPaths[0], test.wantWarning.Error(), "data-root discovery may be incomplete", "verify daemon configuration and access"} {
				if !strings.Contains(report.Warnings[0], expected) {
					t.Errorf("warning missing %q: %s", expected, report.Warnings[0])
				}
			}
		})
	}
}

func TestDiscoverDataRootMalformedConfigContinues(t *testing.T) {
	root := t.TempDir()
	malformed := filepath.Join(root, "invalid-daemon.json")
	valid := filepath.Join(root, "valid-daemon.json")
	if err := os.WriteFile(malformed, []byte(`{"data-root":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valid, []byte(`{"data-root":"/srv/docker"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var report Report
	discoverDataRoot(&report, []string{malformed, valid}, nil, readConfig, func([]string) string {
		t.Fatal("valid data root should not need a fallback")
		return ""
	})
	if report.Config != valid || report.DataRoot != "/srv/docker" || len(report.Warnings) != 1 {
		t.Fatalf("unexpected discovery after malformed config: %+v", report)
	}
	for _, expected := range []string{malformed, "unexpected end of JSON input", "verify daemon configuration and access"} {
		if !strings.Contains(report.Warnings[0], expected) {
			t.Errorf("malformed config warning missing %q: %v", expected, report.Warnings)
		}
	}
}

func TestDiscoverDataRootPreservesEveryConfigFailure(t *testing.T) {
	paths := []string{"/first/daemon.json", "/second/daemon.json"}
	var report Report
	discoverDataRoot(&report, paths, nil,
		func(path string) (daemonConfig, error) {
			return daemonConfig{}, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}
		}, func([]string) string { return "" })
	if len(report.Warnings) != len(paths) || report.Config != "" || report.DataRoot != "" {
		t.Fatalf("configuration failures lost or selected as successful: %+v", report)
	}
	for index, path := range paths {
		if !strings.Contains(report.Warnings[index], path) {
			t.Errorf("warning %d missing failed config %q: %s", index, path, report.Warnings[index])
		}
	}
}

func TestConfigWarningTextEscapingAndJSON(t *testing.T) {
	configPath := "/config/daemon\n\x1b[31m.json"
	reason := "read failed\r\x1b[2J"
	report := Report{Runtime: "docker"}
	discoverDataRoot(&report, []string{configPath}, nil,
		func(string) (daemonConfig, error) { return daemonConfig{}, errors.New(reason) },
		func([]string) string { return "" })
	var output bytes.Buffer
	if err := printReport(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`/config/daemon\n\x1b[31m.json`, `read failed\r\x1b[2J`, "Warning:", "data-root discovery may be incomplete"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("text missing %q: %q", expected, output.String())
		}
	}
	if strings.ContainsAny(output.String(), "\r\x1b") {
		t.Fatalf("raw terminal controls in config warning: %q", output.String())
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Warnings) != 1 || decoded.Warnings[0] != report.Warnings[0] || !strings.Contains(decoded.Warnings[0], configPath) || !strings.Contains(decoded.Warnings[0], reason) {
		t.Fatalf("JSON changed raw config warning: %s", encoded)
	}
}
