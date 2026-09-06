package disk

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestScanRanksFilesAndClassifiesThem(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var", "log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "var", "log", "app.log"), []byte("12345"), 0o644); err != nil {
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

func TestMeasureGrowth(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "growing.log")
	if err := os.WriteFile(path, []byte("start"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []File{{Path: path, Size: 5, Kind: "log"}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, _ = file.WriteString("growth")
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
		{Path: "/large.log", Size: 1000},
		{Path: "/medium.log", Size: 500},
		{Path: "/fast-small.log", Size: 10, Growth: 200},
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
	groups := [][]File{{{Path: firstPath, Size: 5}}, {{Path: secondPath, Size: 5}}}
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
		{Path: "/largest", Size: 1000, ModifiedAt: 1},
		{Path: "/second-largest", Size: 900, ModifiedAt: 2},
		{Path: "/recent", Size: 1, ModifiedAt: 100},
		{Path: "/older", Size: 2, ModifiedAt: 3},
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

func appendAfter(path string, delay time.Duration) {
	time.Sleep(delay)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return
	}
	_, _ = file.WriteString("growth")
	_ = file.Close()
}
