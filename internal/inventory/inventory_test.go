package inventory

import (
	"bytes"
	"strings"
	"testing"

	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/mount"
)

func TestPressureStatus(t *testing.T) {
	tests := []struct {
		used, inodes float64
		want         string
	}{
		{used: 89.9, inodes: 20, want: "OK"},
		{used: 90, inodes: 20, want: "WARNING"},
		{used: 20, inodes: 95, want: "CRITICAL"},
	}
	for _, test := range tests {
		if got := pressureStatus(test.used, test.inodes); got != test.want {
			t.Fatalf("pressureStatus(%v, %v) = %q, want %q", test.used, test.inodes, got, test.want)
		}
	}
}

func TestPrintTableSortsRiskFirst(t *testing.T) {
	entries := []Entry{
		{Status: "OK", Mount: "/data", Source: "/dev/sdb1", Type: "xfs", Kind: "local", Mode: "rw", TotalBytes: 100, UsedBytes: 50, FreeBytes: 50, AvailableBytes: 50, UsedPercent: 50, PressurePercent: 50, InodePercent: 10},
		{Status: "CRITICAL", Mount: "/", Source: "/dev/sda1", Type: "ext4", Kind: "local", Mode: "rw", TotalBytes: 100, UsedBytes: 94, FreeBytes: 6, AvailableBytes: 4, UsedPercent: 94, PressurePercent: 96, InodePercent: 20},
	}
	var output bytes.Buffer
	if err := Print(&output, entries, false); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{"STATUS", "MOUNT", "SOURCE", "AVAIL", "INODE%", "CRITICAL", "/dev/sda1", "/dev/sdb1"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("expected %q in table:\n%s", expected, text)
		}
	}
	if strings.Index(text, "/dev/sda1") > strings.Index(text, "/dev/sdb1") {
		t.Fatalf("expected critical filesystem first:\n%s", text)
	}
}

func TestPrintJSONIncludesRawMetrics(t *testing.T) {
	entries := []Entry{{Status: "WARNING", Mount: "/data", Source: "/dev/sdb1", Type: "xfs", Kind: "local", Mode: "ro", TotalBytes: 100, UsedPercent: 89, PressurePercent: 91.5}}
	var output bytes.Buffer
	if err := Print(&output, entries, true); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, expected := range []string{`"status": "WARNING"`, `"mount": "/data"`, `"available_bytes": 0`, `"pressure_percent": 91.5`, `"mode": "ro"`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("expected %q in JSON:\n%s", expected, text)
		}
	}
}

func TestCollectDoesNotProbeRemoteMounts(t *testing.T) {
	oldList, oldUsage := listMounts, filesystemUsage
	t.Cleanup(func() { listMounts, filesystemUsage = oldList, oldUsage })
	listMounts = func() ([]mount.Info, error) {
		return []mount.Info{
			{Path: "/", Source: "/dev/sda1", Type: "ext4", Kind: mount.KindLocal},
			{Path: "/backup", Source: "server:/backup", Type: "nfs4", Kind: mount.KindRemote},
		}, nil
	}
	usageCalls := 0
	filesystemUsage = func(path string) (disk.Filesystem, error) {
		usageCalls++
		if path == "/backup" {
			t.Fatal("remote mount was probed")
		}
		return disk.Filesystem{Path: path, TotalBytes: 100, AvailableBytes: 50, FreeBytes: 60, PressurePercent: 50}, nil
	}
	entries, err := Collect()
	if err != nil {
		t.Fatal(err)
	}
	if usageCalls != 1 || len(entries) != 2 || entries[1].Status != "REMOTE" {
		t.Fatalf("unexpected inventory: calls=%d entries=%+v", usageCalls, entries)
	}
}

func TestMarkSharedAPFSCapacity(t *testing.T) {
	entries := []Entry{
		{Type: "apfs", TotalBytes: 100, FreeBytes: 40, AvailableBytes: 40},
		{Type: "apfs", TotalBytes: 100, FreeBytes: 40, AvailableBytes: 40},
		{Type: "ext4", TotalBytes: 100, FreeBytes: 40, AvailableBytes: 40},
	}
	markSharedCapacity(entries)
	if !entries[0].SharedCapacity || !entries[1].SharedCapacity || entries[2].SharedCapacity {
		t.Fatalf("unexpected shared-capacity markers: %+v", entries)
	}
}
