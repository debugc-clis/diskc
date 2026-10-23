package health

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Finding struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Message  string `json:"message"`
}

type Snapshot struct {
	Device DeviceStats
	Found  bool
}

type DeviceStats struct {
	Name       string
	Reads      uint64
	Writes     uint64
	ReadBytes  uint64
	WriteBytes uint64
	IOTicks    uint64
}

func TakeSnapshot(device uint64) Snapshot {
	stats, found := readDiskStats()[deviceKey(device)]
	return Snapshot{Device: stats, Found: found}
}

func Findings(root string, device uint64, before Snapshot, seconds float64) []Finding {
	findings := FilesystemFindings(root, device, before, seconds)
	return append(findings, HostFindings()...)
}

func FilesystemFindings(root string, device uint64, before Snapshot, seconds float64) []Finding {
	findings := mountFindings(root)
	if before.Found && seconds > 0 {
		if after, ok := readDiskStats()[deviceKey(device)]; ok {
			findings = append(findings, ioFindings(before.Device, after, seconds)...)
		}
	}
	return findings
}

func HostFindings() []Finding {
	findings := psiFindings()
	return append(findings, raidFindings()...)
}

func deviceKey(device uint64) uint64 {
	major := ((device >> 8) & 0xfff) | ((device >> 32) & 0xfffff000)
	minor := (device & 0xff) | ((device >> 12) & 0xffffff00)
	return major<<32 | minor
}

func readDiskStats() map[uint64]DeviceStats {
	file, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil
	}
	defer file.Close()
	return parseDiskStats(file)
}

func parseDiskStats(reader io.Reader) map[uint64]DeviceStats {
	stats := make(map[uint64]DeviceStats)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 {
			continue
		}
		major, majorErr := strconv.ParseUint(fields[0], 10, 64)
		minor, minorErr := strconv.ParseUint(fields[1], 10, 64)
		reads, readsErr := strconv.ParseUint(fields[3], 10, 64)
		readSectors, readErr := strconv.ParseUint(fields[5], 10, 64)
		writes, writesErr := strconv.ParseUint(fields[7], 10, 64)
		writeSectors, writeErr := strconv.ParseUint(fields[9], 10, 64)
		ioTicks, ticksErr := strconv.ParseUint(fields[12], 10, 64)
		if majorErr != nil || minorErr != nil || readsErr != nil || readErr != nil || writesErr != nil || writeErr != nil || ticksErr != nil {
			continue
		}
		stats[major<<32|minor] = DeviceStats{Name: fields[2], Reads: reads, Writes: writes, ReadBytes: readSectors * 512, WriteBytes: writeSectors * 512, IOTicks: ioTicks}
	}
	return stats
}

func ioFindings(before, after DeviceStats, seconds float64) []Finding {
	readBytes, readBytesOK := counterDelta(before.ReadBytes, after.ReadBytes)
	writeBytes, writeBytesOK := counterDelta(before.WriteBytes, after.WriteBytes)
	reads, readsOK := counterDelta(before.Reads, after.Reads)
	writes, writesOK := counterDelta(before.Writes, after.Writes)
	ioTicks, ticksOK := counterDelta(before.IOTicks, after.IOTicks)
	if !readBytesOK || !writeBytesOK || !readsOK || !writesOK || !ticksOK || seconds <= 0 {
		return nil
	}
	busy := float64(ioTicks) / (seconds * 10)
	if readBytes == 0 && writeBytes == 0 && busy < 1 {
		return nil
	}
	severity := "info"
	if busy >= 90 {
		severity = "warning"
	}
	return []Finding{{
		Severity: severity,
		Kind:     "device-io",
		Message:  fmt.Sprintf("%s: %.1f%% busy, %.1f MB/s read, %.1f MB/s written, %.0f read IOPS, %.0f write IOPS", after.Name, busy, float64(readBytes)/seconds/1024/1024, float64(writeBytes)/seconds/1024/1024, float64(reads)/seconds, float64(writes)/seconds),
	}}
}

func counterDelta(before, after uint64) (uint64, bool) {
	if after < before {
		return 0, false
	}
	return after - before, true
}

func mountFindings(root string) []Finding {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer file.Close()
	return mountFindingsFrom(file, root)
}

func mountFindingsFrom(reader io.Reader, root string) []Finding {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	bestPath := ""
	bestOptions := ""
	bestType := ""
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		pre := strings.Fields(parts[0])
		post := strings.Fields(parts[1])
		if len(pre) < 6 || len(post) < 1 {
			continue
		}
		path := unescape(pre[4])
		if !pathContains(path, root) {
			continue
		}
		if len(path) <= len(bestPath) {
			continue
		}
		bestPath, bestOptions, bestType = path, pre[5], post[0]
	}
	if bestPath == "" {
		return nil
	}
	findings := make([]Finding, 0, 2)
	if hasOption(bestOptions, "ro") {
		findings = append(findings, Finding{Severity: "critical", Kind: "read-only-mount", Message: fmt.Sprintf("%s is mounted read-only (%s)", bestPath, bestType)})
	}
	switch bestType {
	case "btrfs", "zfs":
		findings = append(findings, Finding{Severity: "info", Kind: "copy-on-write", Message: fmt.Sprintf("%s uses %s; snapshots or copy-on-write metadata can consume space beyond ordinary file totals", bestPath, bestType)})
	case "overlay":
		findings = append(findings, Finding{Severity: "info", Kind: "container-overlay", Message: fmt.Sprintf("%s is an overlay filesystem; inspect container writable layers and volumes", bestPath)})
	}
	return findings
}

func pathContains(parent, child string) bool {
	if parent == string(os.PathSeparator) {
		return strings.HasPrefix(child, parent)
	}
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func hasOption(options, target string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == target {
			return true
		}
	}
	return false
}

func psiFindings() []Finding {
	contents, err := os.ReadFile("/proc/pressure/io")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(contents), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, "avg10=") {
				continue
			}
			value, err := strconv.ParseFloat(strings.TrimPrefix(field, "avg10="), 64)
			if err == nil && value >= 10 {
				severity := "warning"
				if value >= 25 {
					severity = "critical"
				}
				return []Finding{{Severity: severity, Kind: "io-pressure", Message: fmt.Sprintf("I/O pressure is %.2f%% over 10 seconds; tasks are stalled waiting for storage", value)}}
			}
		}
	}
	return nil
}

func raidFindings() []Finding {
	contents, err := os.ReadFile("/proc/mdstat")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(contents), "\n") {
		if !strings.Contains(line, "[") || !strings.Contains(line, "]") || !strings.Contains(line, "_") {
			continue
		}
		return []Finding{{Severity: "critical", Kind: "raid-degraded", Message: "software RAID reports a missing or failed member"}}
	}
	return nil
}

func unescape(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}
