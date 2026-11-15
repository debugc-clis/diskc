package docker

import (
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
	"time"

	"github.com/debugc-clis/diskc/internal/disk"
)

func missingDiscoveryConfig(string) (daemonConfig, error) {
	return daemonConfig{}, os.ErrNotExist
}

func TestDiscoverPathsMissingOptionalQuiet(t *testing.T) {
	var report Report
	discoverPaths(&report, []string{"/config"}, []string{"", "/missing"}, []string{"", "/missing.sock"},
		missingDiscoveryConfig, func(path string) (os.FileInfo, error) {
			if path == "" {
				t.Fatal("empty optional path was probed")
			}
			return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
		})
	if len(report.Warnings) != 0 || len(report.Paths) != 0 || report.DataRoot != "" || report.Socket != "" {
		t.Fatalf("missing optional paths produced evidence or warnings: %+v", report)
	}
}

func TestDiscoverPathsRetainsFailuresAndContinues(t *testing.T) {
	const rootless = "/home/operator/.local/share/docker"
	const socket = "/run/user/1000/docker.sock"
	failures := map[string]error{
		"/var/lib/docker":      os.ErrPermission,
		"/var/lib/containerd":  syscall.EIO,
		"/var/log/docker":      os.ErrPermission,
		"/var/run/docker.sock": syscall.EIO,
	}
	var report Report
	discoverPaths(&report, nil, []string{"/var/lib/docker", rootless, "/unneeded"},
		[]string{"/var/run/docker.sock", socket, "/unneeded.sock"}, missingDiscoveryConfig,
		func(path string) (os.FileInfo, error) {
			if strings.HasPrefix(path, "/unneeded") {
				t.Fatalf("probed after first successful candidate: %s", path)
			}
			if err := failures[path]; err != nil {
				return nil, &os.PathError{Op: "stat", Path: path, Err: err}
			}
			return nil, nil
		})
	if report.DataRoot != rootless || report.Socket != socket || len(report.Paths) != 5 || len(report.Warnings) != 4 {
		t.Fatalf("discovery failed to retain candidates or continue: %+v", report)
	}
	for _, path := range report.Paths {
		failure := failures[path.Path]
		if failure != nil {
			if !strings.Contains(path.Error, failure.Error()) || path.ScanScope != nil || len(path.Files) != 0 || path.Usage.AvailableBytes != 0 {
				t.Errorf("failed candidate has missing error or fabricated evidence: %+v", path)
			}
		} else if path.Error != "" {
			t.Errorf("visible candidate marked unavailable: %+v", path)
		}
	}
	for path, failure := range failures {
		matched := false
		for _, warning := range report.Warnings {
			if strings.Contains(warning, path) && strings.Contains(warning, failure.Error()) &&
				strings.Contains(warning, "discovery may be incomplete") && strings.Contains(warning, "verify daemon paths and access") {
				matched = true
			}
		}
		if !matched {
			t.Errorf("missing actionable warning for %s: %v", path, report.Warnings)
		}
	}
}

func TestDiscoverPathsConfiguredRootAndDuplicateCandidates(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "configured"}[configured], func(t *testing.T) {
			calls := map[string]int{}
			var report Report
			read := missingDiscoveryConfig
			if configured {
				read = func(string) (daemonConfig, error) { return daemonConfig{DataRoot: "/var/lib/containerd"}, nil }
			}
			discoverPaths(&report, []string{"/daemon.json"},
				[]string{"/var/lib/containerd", "/var/lib/containerd", "/visible", "/later"}, nil, read,
				func(path string) (os.FileInfo, error) {
					calls[path]++
					switch path {
					case "/var/lib/containerd":
						if configured {
							t.Fatal("configured root should be inspected later by Run")
						}
						return nil, os.ErrPermission
					case "/visible":
						if configured {
							t.Fatal("configured root should bypass fallback candidates")
						}
						return nil, nil
					case "/later":
						t.Fatal("later fallback should not be probed after success")
					}
					return nil, os.ErrNotExist
				})
			if configured {
				if report.DataRoot != "/var/lib/containerd" || report.Config != "/daemon.json" || len(report.Paths) != 1 || len(report.Warnings) != 0 {
					t.Fatalf("configured root behavior changed: %+v", report)
				}
			} else {
				if calls["/var/lib/containerd"] != 1 || report.DataRoot != "/visible" || len(report.Paths) != 2 || len(report.Warnings) != 1 || report.Paths[0].Error == "" {
					t.Fatalf("duplicate failed candidate was lost or reprobed: calls=%v report=%+v", calls, report)
				}
			}
		})
	}
}

func TestDiscoverPathsRealMetadata(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	var report Report
	discoverPaths(&report, nil, []string{missing, root}, []string{missing}, missingDiscoveryConfig, os.Stat)
	if report.DataRoot != root || report.Socket != "" {
		t.Fatalf("real metadata discovery failed: %+v", report)
	}
	found := false
	for _, path := range report.Paths {
		if path.Path == missing {
			t.Errorf("missing candidate retained as evidence: %+v", path)
		}
		if path.Path == root {
			found = true
			if path.Error != "" {
				t.Errorf("visible directory marked unavailable: %+v", path)
			}
		}
	}
	if !found {
		t.Fatalf("visible directory lost: %+v", report)
	}
}

func TestRunDiscoveryFailuresRemainUnavailableInTextAndJSON(t *testing.T) {
	oldDiscover, oldUsage, oldScan := discover, filesystemUsage, scan
	oldMeasure, oldAttribute := measureGrowthGroups, attributeWriters
	t.Cleanup(func() {
		discover, filesystemUsage, scan = oldDiscover, oldUsage, oldScan
		measureGrowthGroups, attributeWriters = oldMeasure, oldAttribute
	})
	failed := "/denied\n\x1b[31m"
	reason := errors.New("metadata denied\r\x1b[2J")
	fixture := Report{Runtime: "docker"}
	discoverPaths(&fixture, nil, []string{failed, "/visible"}, []string{"/denied.sock"}, missingDiscoveryConfig,
		func(path string) (os.FileInfo, error) {
			switch path {
			case failed, "/denied.sock":
				return nil, reason
			case "/visible":
				return nil, nil
			}
			return nil, os.ErrNotExist
		})
	discover = func() Report {
		report := fixture
		report.Paths = append([]PathReport(nil), fixture.Paths...)
		return report
	}
	usageCalls, scanCalls := 0, 0
	filesystemUsage = func(path string) (disk.Filesystem, error) {
		usageCalls++
		if path != "/visible" {
			t.Fatalf("failed metadata candidate reached filesystem probe: %q", path)
		}
		return disk.Filesystem{Path: path, Device: 1, AvailableBytes: 4096}, nil
	}
	scan = func(path string, _ uint64, _, _ int) ([]disk.File, []disk.Directory, disk.ScanScope, []string, error) {
		scanCalls++
		if path != "/visible" {
			t.Fatalf("failed metadata candidate reached scan: %q", path)
		}
		return []disk.File{}, nil, disk.ScanScope{Root: path}, nil, nil
	}
	measureGrowthGroups = func(groups [][]disk.File, filesystems []*disk.Filesystem, interval time.Duration) error {
		if len(groups) != 1 || len(filesystems) != 1 || interval != 0 {
			t.Fatalf("unavailable candidate reached growth sampling: groups=%d filesystems=%d interval=%s", len(groups), len(filesystems), interval)
		}
		return nil
	}
	attributeWriters = func(files []disk.File) []string {
		if len(files) != 0 {
			t.Fatalf("unavailable candidate fabricated writer evidence: %+v", files)
		}
		return nil
	}
	var output bytes.Buffer
	if err := Run([]string{"--sample", "0"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`/denied\n\x1b[31m`, `metadata denied\r\x1b[2J`, "Unavailable: cannot inspect during discovery:", "verify daemon paths and access"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("text missing %q: %q", want, output.String())
		}
	}
	if strings.ContainsAny(output.String(), "\r\x1b") || strings.Count(output.String(), "Pressure:") != 1 || strings.Contains(output.String(), "Socket:") {
		t.Fatalf("text fabricated pressure/socket evidence or emitted terminal controls: %q", output.String())
	}
	output.Reset()
	if err := Run([]string{"--sample", "0", "--json"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result Report
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Socket != "" || len(result.Paths) != 2 || !reflect.DeepEqual(result.Warnings, fixture.Warnings) {
		t.Fatalf("JSON lost discovery failure evidence: %s", output.String())
	}
	path := result.Paths[0]
	if path.Path != failed || path.Error != fixture.Paths[0].Error || path.ScanScope != nil || len(path.Files) != 0 || len(path.Growing) != 0 || len(path.Directories) != 0 || path.Usage.AvailableBytes != 0 {
		t.Fatalf("JSON changed raw errors or fabricated scan evidence: %+v", path)
	}
	if result.Paths[1].ScanScope == nil || result.Paths[1].Usage.AvailableBytes != 4096 || usageCalls != 2 || scanCalls != 2 {
		t.Fatalf("accessible candidate did not continue: result=%+v usage=%d scan=%d", result, usageCalls, scanCalls)
	}
}

func TestDiscoverPathsSocketFailureDoesNotConfirmSocketOrDockerAbsence(t *testing.T) {
	report := Report{Runtime: "docker"}
	discoverPaths(&report, nil, nil, []string{"/denied.sock", "/denied.sock", "/later.sock"}, missingDiscoveryConfig,
		func(path string) (os.FileInfo, error) {
			if strings.HasSuffix(path, ".sock") {
				return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrPermission}
			}
			return nil, os.ErrNotExist
		})
	if report.Socket != "" || len(report.Paths) != 0 || len(report.Warnings) != 2 {
		t.Fatalf("failed sockets treated as discovered or storage paths: %+v", report)
	}
	var output bytes.Buffer
	if err := printReport(&output, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "No Docker storage paths confirmed.") || !strings.Contains(output.String(), "Warning:") || strings.Contains(output.String(), "Socket:") {
		t.Fatalf("ambiguous absence or fabricated socket: %s", output.String())
	}
}
