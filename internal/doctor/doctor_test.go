package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/mount"
)

func TestMatchingMountUsesLongestPathBoundary(t *testing.T) {
	mounts := []mount.Info{
		{Path: "/", Kind: mount.KindLocal},
		{Path: "/data", Kind: mount.KindLocal},
		{Path: "/database", Kind: mount.KindRemote},
	}
	if got := matchingMount("/data/app", mounts); got == nil || got.Path != "/data" {
		t.Fatalf("wrong mount for /data/app: %+v", got)
	}
	if got := matchingMount("/databases", mounts); got == nil || got.Path != "/" {
		t.Fatalf("mount prefix ignored path boundary: %+v", got)
	}
}

func TestUsageChecksWarnOnPressureAndInodes(t *testing.T) {
	checks := usageChecks(disk.Filesystem{PressurePercent: 92.6, InodePercent: 96, AvailableBytes: 1024})
	if len(checks) != 2 || checks[0].Status != "WARNING" || checks[1].Status != "CRITICAL" {
		t.Fatalf("unexpected checks: %+v", checks)
	}
	if checks[0].Advice == "" || checks[1].Advice == "" {
		t.Fatalf("missing next steps: %+v", checks)
	}
}

func TestRunAcceptsPathBeforeJSONFlag(t *testing.T) {
	var output bytes.Buffer
	if err := Run([]string{t.TempDir(), "--json"}, &output); err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, output.String())
	}
	if report.Defaults.Top != 20 || report.Filesystem == nil || len(report.Checks) == 0 {
		t.Fatalf("incomplete diagnosis: %+v", report)
	}
}

func TestRunReportsMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	var output bytes.Buffer
	if err := Run([]string{missing}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "[WARNING] filesystem access") || !strings.Contains(output.String(), "issue(s) found") {
		t.Fatalf("missing path not diagnosed: %s", output.String())
	}
}

func TestRemoteMountIsNotProbed(t *testing.T) {
	oldList, oldUsage := listMounts, filesystemUsage
	t.Cleanup(func() { listMounts, filesystemUsage = oldList, oldUsage })
	listMounts = func() ([]mount.Info, error) {
		return []mount.Info{{Path: "/", Kind: mount.KindLocal}, {Path: "/backup", Kind: mount.KindRemote}}, nil
	}
	filesystemUsage = func(string) (disk.Filesystem, error) {
		t.Fatal("remote mount must not be probed")
		return disk.Filesystem{}, errors.New("unreachable")
	}
	report := Collect("/backup/archive")
	if report.Filesystem != nil || report.Mount == nil || report.Mount.Kind != mount.KindRemote {
		t.Fatalf("unexpected remote report: %+v", report)
	}
}
