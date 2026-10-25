package disk

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScanRanksFilesAndClassifiesThem(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var", "log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "var", "log", "app.log"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "small"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	filesystem, err := FilesystemUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	files, directories, warnings, err := Scan(root, filesystem.Device, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || len(files) != 2 || len(directories) == 0 {
		t.Fatalf("unexpected scan result: files=%d directories=%d warnings=%v", len(files), len(directories), warnings)
	}
	if files[0].Kind != "log" || files[0].Path != filepath.Join(root, "var", "log", "app.log") {
		t.Fatalf("unexpected top file: %+v", files[0])
	}
}

func TestFilesystemUsageSeparatesFreeAndAvailable(t *testing.T) {
	filesystem, err := FilesystemUsage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if filesystem.UsedBytes+filesystem.FreeBytes != filesystem.TotalBytes {
		t.Fatalf("used + free must equal total: %+v", filesystem)
	}
	if filesystem.AvailableBytes > filesystem.FreeBytes || filesystem.ReservedBytes != filesystem.FreeBytes-filesystem.AvailableBytes {
		t.Fatalf("invalid available/reserved metrics: %+v", filesystem)
	}
	if filesystem.PressurePercent < filesystem.UsedPercent {
		t.Fatalf("pressure cannot be below actual usage: %+v", filesystem)
	}
}

func TestMeasureGrowth(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "growing.log")
	if err := os.WriteFile(path, []byte("start"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []File{{Path: path, AllocatedBytes: 4096, ApparentBytes: 5, Kind: "log"}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, _ = file.Write(make([]byte, 1024*1024))
			_ = file.Close()
		}
	}()
	if err := MeasureGrowth(files, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if files[0].Growth <= 0 {
		t.Fatalf("expected positive growth, got %f", files[0].Growth)
	}
}

func TestSelectReportFilesIncludesFastSmallFile(t *testing.T) {
	files := []File{
		{Path: "/large.log", AllocatedBytes: 1000},
		{Path: "/medium.log", AllocatedBytes: 500},
		{Path: "/fast-small.log", AllocatedBytes: 10, Growth: 200},
	}
	largest, growing := SelectReportFiles(files, 1)
	if len(largest) != 1 || largest[0].Path != "/large.log" {
		t.Fatalf("unexpected largest files: %+v", largest)
	}
	if len(growing) != 1 || growing[0].Path != "/fast-small.log" {
		t.Fatalf("unexpected growing files: %+v", growing)
	}
}

func TestMeasureGrowthGroupsUsesOneSharedWindow(t *testing.T) {
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.log")
	secondPath := filepath.Join(root, "second.log")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte("start"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	groups := [][]File{{{Path: firstPath, AllocatedBytes: 4096}}, {{Path: secondPath, AllocatedBytes: 4096}}}
	var writers sync.WaitGroup
	writers.Add(2)
	go func() {
		defer writers.Done()
		appendAfter(firstPath, 30*time.Millisecond)
	}()
	go func() {
		defer writers.Done()
		appendAfter(secondPath, 180*time.Millisecond)
	}()
	if err := MeasureGrowthGroups(groups, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	writers.Wait()
	if groups[0][0].Growth <= 0 {
		t.Fatalf("expected first group to grow, got %f", groups[0][0].Growth)
	}
	if groups[1][0].Growth != 0 {
		t.Fatalf("second group was sampled in a later window: %f", groups[1][0].Growth)
	}
}

func TestPruneCandidatesKeepsLargeAndRecentFiles(t *testing.T) {
	files := []File{
		{Path: "/largest", AllocatedBytes: 1000, ModifiedAt: 1},
		{Path: "/second-largest", AllocatedBytes: 900, ModifiedAt: 2},
		{Path: "/recent", AllocatedBytes: 1, ModifiedAt: 100},
		{Path: "/older", AllocatedBytes: 2, ModifiedAt: 3},
	}
	result := pruneCandidates(files, 2)
	seen := make(map[string]bool)
	for _, file := range result {
		seen[file.Path] = true
	}
	if !seen["/largest"] || !seen["/recent"] {
		t.Fatalf("expected largest and recent candidates, got %+v", result)
	}
}

func TestScanUsesAllocatedBytesForSparseFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sparse.img")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1 << 30); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	filesystem, err := FilesystemUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	files, directories, _, err := Scan(root, filesystem.Device, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ApparentBytes != 1<<30 {
		t.Fatalf("unexpected sparse file metadata: %+v", files)
	}
	if files[0].AllocatedBytes >= files[0].ApparentBytes {
		t.Fatalf("expected allocated bytes below apparent bytes: %+v", files[0])
	}
	if len(directories) != 1 || directories[0].AllocatedBytes != files[0].AllocatedBytes {
		t.Fatalf("directory should use allocated bytes: %+v", directories)
	}
}

func TestScanCountsHardLinksOnce(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.log")
	second := filepath.Join(root, "second.log")
	if err := os.WriteFile(first, make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Fatal(err)
	}
	filesystem, err := FilesystemUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	files, directories, warnings, err := Scan(root, filesystem.Device, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected one inode candidate, got %+v", files)
	}
	if len(directories) != 1 || directories[0].AllocatedBytes != files[0].AllocatedBytes {
		t.Fatalf("hard link was double-counted: files=%+v directories=%+v", files, directories)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "duplicate hard-link") {
		t.Fatalf("expected hard-link warning, got %v", warnings)
	}
}

func TestUpdateTrendUsesAvailableCapacity(t *testing.T) {
	filesystem := Filesystem{FreeBytes: 10_000, AvailableBytes: 1_000}
	UpdateTrend(100, &filesystem)
	if filesystem.FullInSeconds != 10 {
		t.Fatalf("expected ETA from available bytes, got %+v", filesystem)
	}
}

func TestUpdateTrendNetRates(t *testing.T) {
	for _, test := range []struct {
		name string
		rate float64
		eta  float64
	}{
		{"net growth", 200, 5},
		{"net recovery", -200, 0},
		{"zero", 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			filesystem := Filesystem{AvailableBytes: 1000, FullInSeconds: 99}
			UpdateTrend(test.rate, &filesystem)
			if filesystem.GrowthBytesPerSecond != test.rate || filesystem.FullInSeconds != test.eta {
				t.Fatalf("unexpected trend: %+v", filesystem)
			}
		})
	}
}

func TestMeasureGrowthGroupsWithFilesystemsSharesDevice(t *testing.T) {
	for _, test := range []struct {
		name      string
		final     uint64
		isGrowing bool
	}{
		{name: "growth outside candidate set", final: 800, isGrowing: true},
		{name: "net recovery", final: 1200},
		{name: "no net change", final: 1000},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := sampleFilesystemUsage
			t.Cleanup(func() { sampleFilesystemUsage = original })
			calls := 0
			sampleFilesystemUsage = func(path string) (Filesystem, error) {
				calls++
				available := uint64(1000)
				if calls == 2 {
					available = test.final
				}
				return Filesystem{Path: path, Device: 7, AvailableBytes: available}, nil
			}
			first := Filesystem{Path: "/first", Device: 7}
			second := Filesystem{Path: "/first/nested", Device: 7}
			if err := MeasureGrowthGroupsWithFilesystems(nil, []*Filesystem{&first, &second}, time.Millisecond); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || first.GrowthBytesPerSecond != second.GrowthBytesPerSecond || first.AvailableBytes != second.AvailableBytes {
				t.Fatalf("expected one shared device sample: calls=%d first=%+v second=%+v", calls, first, second)
			}
			if (first.GrowthBytesPerSecond > 0) != test.isGrowing || (first.FullInSeconds > 0) != test.isGrowing {
				t.Fatalf("incorrect net growth or ETA: %+v", first)
			}
			if first.GrowthBasis != "filesystem_available_bytes" || second.Path != "/first/nested" {
				t.Fatalf("sample basis or path lost: %+v %+v", first, second)
			}
		})
	}
}

func appendAfter(path string, delay time.Duration) {
	time.Sleep(delay)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return
	}
	_, _ = file.Write(make([]byte, 1024*1024))
	_ = file.Close()
}
