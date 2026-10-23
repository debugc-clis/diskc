package health

import (
	"strings"
	"testing"
)

func TestIOFindingsReportsBusyDevice(t *testing.T) {
	before := DeviceStats{Name: "sda", Reads: 10, Writes: 20, ReadBytes: 1024, WriteBytes: 2048, IOTicks: 100}
	after := DeviceStats{Name: "sda", Reads: 110, Writes: 220, ReadBytes: 1024 * 1024, WriteBytes: 2 * 1024 * 1024, IOTicks: 1100}
	findings := ioFindings(before, after, 1)
	if len(findings) != 1 || findings[0].Severity != "warning" || findings[0].Kind != "device-io" {
		t.Fatalf("unexpected findings: %+v", findings)
	}
}

func TestDeviceKeyUsesLinuxMajorMinorEncoding(t *testing.T) {
	device := uint64(8<<8 | 1)
	if got := deviceKey(device); got != 8<<32|1 {
		t.Fatalf("unexpected device key: %d", got)
	}
}

func TestIOFindingsIgnoresCounterReset(t *testing.T) {
	before := DeviceStats{Name: "sda", Reads: 100, Writes: 100, ReadBytes: 1000, WriteBytes: 1000, IOTicks: 1000}
	after := DeviceStats{Name: "sda", Reads: 1, Writes: 1, ReadBytes: 10, WriteBytes: 10, IOTicks: 10}
	if findings := ioFindings(before, after, 1); len(findings) != 0 {
		t.Fatalf("counter reset should not produce rates: %+v", findings)
	}
}

func TestMountFindingsMatchesRootMountAndExactReadOnlyOption(t *testing.T) {
	mountinfo := "36 25 8:1 / / rw,errors=remount-ro - overlay overlay rw\n"
	findings := mountFindingsFrom(strings.NewReader(mountinfo), "/var/lib")
	if len(findings) != 1 || findings[0].Kind != "container-overlay" {
		t.Fatalf("expected root overlay finding: %+v", findings)
	}
	for _, finding := range findings {
		if finding.Kind == "read-only-mount" {
			t.Fatalf("remount-ro must not be treated as ro: %+v", findings)
		}
	}
}

func TestParseDiskStats(t *testing.T) {
	stats := parseDiskStats(strings.NewReader("8 0 sda 10 0 20 0 30 0 40 0 0 50 0 0 0 0\ninvalid\n"))
	got, found := stats[8<<32]
	if !found || got.Reads != 10 || got.Writes != 30 || got.ReadBytes != 20*512 || got.IOTicks != 50 {
		t.Fatalf("unexpected disk stats: %+v", stats)
	}
}
