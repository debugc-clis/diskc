package disk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScanRanksFilesAndClassifiesThem(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
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

func TestScanFollowsOnlyRequestedRootSymlinks(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "data")
	child := filepath.Join(target, "child")
	outside := filepath.Join(base, "outside")
	for _, path := range []string{child, outside} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, size := range map[string]int{
		filepath.Join(target, "app.log"): 8192,
		filepath.Join(child, "db.data"):  16384,
		filepath.Join(outside, "hidden"): 32768,
	} {
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "alias")
	chain := filepath.Join(base, "chain")
	fileAlias := filepath.Join(base, "file-alias")
	for link, destination := range map[string]string{
		alias:                                "data",
		chain:                                "alias",
		fileAlias:                            filepath.Join("data", "app.log"),
		filepath.Join(target, "linked-dir"):  outside,
		filepath.Join(target, "linked-file"): filepath.Join(outside, "hidden"),
		filepath.Join(target, "loop"):        "loop",
	} {
		if err := os.Symlink(destination, link); err != nil {
			t.Fatal(err)
		}
	}
	filesystem, err := FilesystemUsage(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, root, target string
	}{
		{"directory", alias, target},
		{"chain", chain, target},
		{"intermediate", filepath.Join(alias, "child"), child},
		{"file", fileAlias, filepath.Join(target, "app.log")},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, depth := range []int{0, 1} {
				files, directories, warnings, err := Scan(test.root, filesystem.Device, depth, 10)
				if err != nil {
					t.Fatal(err)
				}
				wantFiles, wantDirectories, wantWarnings, err := Scan(test.target, filesystem.Device, depth, 10)
				if err != nil {
					t.Fatal(err)
				}
				wantCount := 1
				if test.target == target && depth == 1 {
					wantCount = 2
				}
				if len(files) != wantCount || len(warnings) != 0 || len(wantWarnings) != 0 || !reflect.DeepEqual(files, wantFiles) || !reflect.DeepEqual(directories, wantDirectories) {
					t.Fatalf("depth %d: alias scan differs from target or followed descendant links: files=%+v directories=%+v warnings=%v; want files=%+v directories=%+v", depth, files, directories, warnings, wantFiles, wantDirectories)
				}
				for _, file := range files {
					if file.AllocatedBytes <= 0 {
						t.Fatalf("expected allocated disk blocks: %+v", file)
					}
				}
			}
			files, directories, warnings, err := Scan(test.root, filesystem.Device+1, 4, 10)
			if err != nil || len(files) != 0 || len(directories) != 0 || len(warnings) != 0 {
				t.Fatalf("scan crossed device boundary: files=%+v directories=%+v warnings=%v err=%v", files, directories, warnings, err)
			}
		})
	}
}

func TestScanRejectsBrokenOrLoopingRootSymlinks(t *testing.T) {
	base := t.TempDir()
	for _, test := range []struct{ name, destination string }{
		{"broken", "missing"},
		{"loop", "loop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(base, test.name)
			if err := os.Symlink(test.destination, root); err != nil {
				t.Fatal(err)
			}
			files, directories, _, err := Scan(root, 0, 4, 10)
			if err == nil || len(files) != 0 || len(directories) != 0 {
				t.Fatalf("invalid root must fail explicitly: files=%+v directories=%+v err=%v", files, directories, err)
			}
		})
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

func TestMeasureGrowthChecksFileIdentityAndAvailability(t *testing.T) {
	for _, test := range []struct {
		name          string
		before        string
		during        string
		wantStatus    string
		wantGrowth    int
		wantFinalSize int64
	}{
		{name: "same file grows", during: "append", wantStatus: "measured", wantGrowth: 1, wantFinalSize: 2 << 20},
		{name: "same file truncates", during: "truncate", wantStatus: "measured", wantGrowth: -1, wantFinalSize: 0},
		{name: "same file unchanged", wantStatus: "measured", wantFinalSize: 1 << 20},
		{name: "larger replacement", during: "replace large", wantStatus: "replaced", wantFinalSize: 2 << 20},
		{name: "smaller replacement", during: "replace small", wantStatus: "replaced", wantFinalSize: 4096},
		{name: "replacement before baseline", before: "replace large", wantStatus: "replaced", wantFinalSize: 2 << 20},
		{name: "removed", during: "remove", wantStatus: "unavailable", wantFinalSize: 1 << 20},
		{name: "latest baseline retained after removal", before: "append", during: "remove", wantStatus: "unavailable", wantFinalSize: 2 << 20},
		{name: "symlink replacement", during: "symlink", wantStatus: "unavailable", wantFinalSize: 1 << 20},
		{name: "directory replacement", during: "directory", wantStatus: "unavailable", wantFinalSize: 1 << 20},
		{name: "missing baseline restored", before: "remove", during: "restore", wantStatus: "unavailable", wantFinalSize: 2 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "app.log")
			if err := os.WriteFile(path, make([]byte, 1<<20), 0o644); err != nil {
				t.Fatal(err)
			}
			filesystem, err := FilesystemUsage(root)
			if err != nil {
				t.Fatal(err)
			}
			files, _, _, err := Scan(root, filesystem.Device, 1, 10)
			if err != nil || len(files) != 1 {
				t.Fatalf("initial scan: files=%+v err=%v", files, err)
			}
			// A prior rate must not survive an unavailable or replaced sample.
			files[0].Growth = 12345
			change := func(action string) {
				t.Helper()
				var err error
				switch action {
				case "":
					return
				case "append":
					var file *os.File
					file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
					if err == nil {
						_, err = file.Write(make([]byte, 1<<20))
						closeErr := file.Close()
						if err == nil {
							err = closeErr
						}
					}
				case "truncate":
					err = os.Truncate(path, 0)
				case "replace large", "replace small", "symlink", "directory":
					// Keep the old inode allocated so the replacement cannot reuse it.
					err = os.Rename(path, path+".old")
					if err == nil {
						switch action {
						case "replace large":
							err = os.WriteFile(path, make([]byte, 2<<20), 0o644)
						case "replace small":
							err = os.WriteFile(path, make([]byte, 4096), 0o644)
						case "symlink":
							err = os.Symlink(path+".old", path)
						case "directory":
							err = os.Mkdir(path, 0o755)
						}
					}
				case "remove":
					err = os.Remove(path)
				case "restore":
					err = os.WriteFile(path, make([]byte, 2<<20), 0o644)
				default:
					t.Fatalf("unknown test action %q", action)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			change(test.before)
			original := sampleFilesystemUsage
			t.Cleanup(func() { sampleFilesystemUsage = original })
			calls := 0
			sampleFilesystemUsage = func(path string) (Filesystem, error) {
				calls++
				if calls == 2 {
					change(test.during)
				}
				return filesystem, nil
			}
			if err := MeasureGrowthGroupsWithFilesystems([][]File{files}, []*Filesystem{&filesystem}, time.Millisecond); err != nil {
				t.Fatal(err)
			}
			file := files[0]
			if file.GrowthStatus != test.wantStatus || file.ApparentBytes != test.wantFinalSize {
				t.Fatalf("unexpected sample: %+v; want status=%s apparent=%d", file, test.wantStatus, test.wantFinalSize)
			}
			if (test.wantGrowth == 0 && file.Growth != 0) || (test.wantGrowth > 0 && file.Growth <= 0) || (test.wantGrowth < 0 && file.Growth >= 0) {
				t.Fatalf("unexpected growth %f for direction %d", file.Growth, test.wantGrowth)
			}
			_, growing := SelectReportFiles(files, 10)
			if test.wantGrowth <= 0 && len(growing) != 0 {
				t.Fatalf("unmeasured/shrinking files must not rank as growing: %+v", growing)
			}
		})
	}
}

func TestMeasureGrowthRejectsScanDeviceChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata, err := regularFileMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	files := []File{{Path: path, identityKnown: true, identity: inodeKey{device: metadata.device + 1, inode: metadata.inode}}}
	if err := MeasureGrowth(files, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if files[0].GrowthStatus != "replaced" || files[0].Growth != 0 {
		t.Fatalf("device change must invalidate the scan identity: %+v", files[0])
	}
}

func TestMeasureGrowthOverlappingRootsKeepIndependentBaselines(t *testing.T) {
	for _, missingFirst := range []bool{false, true} {
		name := "rotation between overlapping baselines"
		if missingFirst {
			name = "unavailable first overlapping baseline"
		}
		t.Run(name, func(t *testing.T) {
			old := fileMetadata{device: 1, inode: 2, allocatedBytes: 4096, apparentBytes: 100}
			current := fileMetadata{device: 1, inode: 3, allocatedBytes: 8192, apparentBytes: 200}
			groups := [][]File{
				{{Path: "/shared/app.log", identityKnown: true, identity: inodeKey{device: old.device, inode: old.inode}}},
				{{Path: "/shared/app.log", identityKnown: true, identity: inodeKey{device: current.device, inode: current.inode}}},
			}
			calls := 0
			sampleFile := func(string) (fileMetadata, error) {
				calls++
				if calls == 1 {
					if missingFirst {
						return fileMetadata{}, os.ErrNotExist
					}
					return old, nil
				}
				return current, nil
			}
			if err := measureGrowthGroupsWithFilesystems(groups, nil, time.Millisecond, sampleFile); err != nil {
				t.Fatal(err)
			}
			wantFirst := "replaced"
			if missingFirst {
				wantFirst = "unavailable"
			}
			if groups[0][0].GrowthStatus != wantFirst || groups[0][0].Growth != 0 {
				t.Fatalf("first baseline lost: %+v; want %s", groups[0][0], wantFirst)
			}
			if groups[1][0].GrowthStatus != "measured" || groups[1][0].Growth != 0 {
				t.Fatalf("second baseline lost: %+v", groups[1][0])
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

func TestGrowthRetainsBaselineIdentityWhenFinalObservationUnavailable(t *testing.T) {
	for _, known := range []bool{false, true} {
		file := File{Path: "/app.log", AllocatedBytes: 512, ApparentBytes: 512, identityKnown: known, identity: inodeKey{device: 1, inode: 10}}
		files := []File{file}
		observed := fileMetadata{device: 2, inode: 20, allocatedBytes: 4096, apparentBytes: 2048, modifiedAt: 42}
		calls := 0
		err := measureGrowthGroupsWithFilesystems([][]File{files}, nil, time.Millisecond, func(string) (fileMetadata, error) {
			calls++
			if calls == 1 {
				return observed, nil
			}
			return fileMetadata{}, os.ErrNotExist
		})
		if err != nil {
			t.Fatal(err)
		}
		device, inode, identityKnown := files[0].Identity()
		if !identityKnown || device != 2 || inode != 20 || files[0].AllocatedBytes != 4096 || files[0].ApparentBytes != 2048 || files[0].GrowthStatus != "unavailable" || files[0].Growth != 0 {
			t.Fatalf("last successful size/identity observation lost (initially known=%t): %+v identity=%d/%d/%t", known, files[0], device, inode, identityKnown)
		}
	}
}

func TestFileIdentityRemainsPrivateInJSON(t *testing.T) {
	encoded, err := json.Marshal(File{Path: "/app.log", identityKnown: true, identity: inodeKey{device: 123, inode: 456}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"identity", "identityKnown", "device", "inode"} {
		if _, found := fields[key]; found {
			t.Fatalf("private identity exposed in JSON: %s", encoded)
		}
	}
}
