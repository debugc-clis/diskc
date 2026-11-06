package disk

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestScanScopeTracksDepthAndObservedTotals(t *testing.T) {
	root := resolvedScopeRoot(t)
	for _, directory := range []string{"child/grandchild", "sibling/deeper"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, size := range map[string]int{
		"root-file": 8192, "child/child-file": 16384,
		"child/grandchild/hidden": 32768, "sibling/deeper/hidden": 65536,
	} {
		if err := os.WriteFile(filepath.Join(root, path), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"linked-file": "root-file", "linked-directory": "child", "broken-link": "absent",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(root, "root-file"), filepath.Join(root, "root-hardlink")); err != nil {
		t.Fatal(err)
	}
	device, err := deviceOf(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, maxDepth := range []int{0, 1} {
		t.Run(fmt.Sprintf("depth-%d", maxDepth), func(t *testing.T) {
			files, directories, scope, warnings, err := ScanWithScope(root, device, maxDepth, 10)
			if err != nil {
				t.Fatal(err)
			}
			wantFiles := maxDepth + 1
			if scope.Root != root || scope.MaxDepth != maxDepth || scope.RegularFiles != wantFiles || len(files) != wantFiles {
				t.Fatalf("unexpected observed scope: %+v, files=%+v", scope, files)
			}
			if scope.SkippedDepthDirectories != 2 || scope.SkippedSymlinks != 3 || scope.InspectionErrors != 0 || scope.SkippedFilesystemDirectories != 0 || scope.SkippedPseudoDirectories != 0 {
				t.Fatalf("unexpected boundary counts: %+v", scope)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "duplicate hard-link") {
				t.Fatalf("hard links should remain deduplicated: %v", warnings)
			}
			var wantAllocated, wantApparent int64
			for _, file := range files {
				if strings.Contains(file.Path, "hidden") {
					t.Fatalf("visited excluded descendant: %+v", file)
				}
				wantAllocated += file.AllocatedBytes
				wantApparent += file.ApparentBytes
			}
			foundRoot := false
			for _, directory := range directories {
				if directory.Path == root {
					foundRoot = true
					if directory.AllocatedBytes != wantAllocated || directory.ApparentBytes != wantApparent {
						t.Fatalf("directory included unobserved or duplicate files: %+v; want %d allocated / %d apparent", directory, wantAllocated, wantApparent)
					}
				}
			}
			if !foundRoot {
				t.Fatal("root directory total missing")
			}
		})
	}
}

func TestScanScopeResolvesRootAliasesAndHandlesFileRoots(t *testing.T) {
	base := resolvedScopeRoot(t)
	root := filepath.Join(base, "data")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	device, err := deviceOf(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{root, file} {
		alias := filepath.Join(base, "alias-"+filepath.Base(target))
		if err := os.Symlink(target, alias); err != nil {
			t.Fatal(err)
		}
		files, directories, scope, warnings, err := ScanWithScope(alias, device, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if scope.Root != target || scope.RegularFiles != 1 || scope.SkippedSymlinks != 0 || scope.InspectionErrors != 0 || len(files) != 1 || len(warnings) != 0 {
			t.Fatalf("root alias should resolve without a skip: %+v files=%+v warnings=%v", scope, files, warnings)
		}
		if target == file && len(directories) != 0 {
			t.Fatalf("file root invented directory totals: %+v", directories)
		}
	}
}

func TestScanScopeCountsFilesystemDirectoryBoundaries(t *testing.T) {
	root := resolvedScopeRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "child", "grandchild"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child", "grandchild", "hidden"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	device, err := deviceOf(root)
	if err != nil {
		t.Fatal(err)
	}
	files, directories, scope, warnings, err := ScanWithScope(root, device+1, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if scope.SkippedFilesystemDirectories != 1 || scope.SkippedDepthDirectories != 0 || scope.RegularFiles != 0 || len(files) != 0 || len(directories) != 0 || len(warnings) != 0 {
		t.Fatalf("expected one boundary, not all hidden descendants: %+v files=%+v directories=%+v warnings=%v", scope, files, directories, warnings)
	}
	_, _, scope, _, err = ScanWithScope(root, device+1, 0, 10)
	if err != nil || scope.SkippedFilesystemDirectories != 0 || scope.SkippedDepthDirectories != 1 {
		t.Fatalf("depth boundary should be classified before inspecting its device: %+v err=%v", scope, err)
	}
}

func TestScanScopeCountsPseudoBoundariesBeforeDepth(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux /proc mount")
	}
	device, err := deviceOf("/proc")
	if err != nil {
		t.Skipf("/proc unavailable: %v", err)
	}
	// Only /proc itself is read: every child directory is a pseudo boundary,
	// even at depth zero. Do not assert exact live-process or directory counts.
	_, _, scope, _, err := ScanWithScope("/proc", device, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if scope.SkippedPseudoDirectories == 0 || scope.SkippedDepthDirectories != 0 || scope.SkippedFilesystemDirectories != 0 {
		t.Fatalf("pseudo boundary should take precedence over depth/device: %+v", scope)
	}
}

func TestScanScopeCountsFilesBeforeCandidatePruning(t *testing.T) {
	root := resolvedScopeRoot(t)
	count := scanCandidateLimit(1) + 2
	for index := 0; index < count; index++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%04d", index)), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	device, err := deviceOf(root)
	if err != nil {
		t.Fatal(err)
	}
	files, _, scope, warnings, err := ScanWithScope(root, device, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if scope.RegularFiles != count || len(files) != scanCandidateLimit(1) || len(warnings) != 1 || !strings.Contains(warnings[0], "candidates") {
		t.Fatalf("scope should retain observed count after pruning: %+v candidates=%d warnings=%v", scope, len(files), warnings)
	}
}

func TestScanScopeCountsErrorsBeyondWarningLimit(t *testing.T) {
	root := resolvedScopeRoot(t)
	count := maxScanWarnings + 2
	for index := 0; index < count; index++ {
		path := filepath.Join(root, fmt.Sprintf("unreadable-%03d", index))
		if err := os.Mkdir(path, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
	}
	if _, err := os.ReadDir(filepath.Join(root, "unreadable-000")); err == nil {
		t.Skip("current user can read mode-000 directories")
	}
	device, err := deviceOf(root)
	if err != nil {
		t.Fatal(err)
	}
	_, _, scope, warnings, err := ScanWithScope(root, device, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if scope.InspectionErrors != count || scope.SkippedDepthDirectories != 0 || len(warnings) != maxScanWarnings+1 || !strings.Contains(warnings[len(warnings)-1], "omitted") {
		t.Fatalf("error count must survive warning cap: %+v warnings=%v", scope, warnings)
	}
}

func resolvedScopeRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
