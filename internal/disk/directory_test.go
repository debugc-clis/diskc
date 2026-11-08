package disk

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDirectoryTrackerPreservesEstimateEvidence(t *testing.T) {
	tracker := newDirectoryTracker(2)
	tracker.add("/exact", 100, 200)
	tracker.add("/small", 10, 20)
	want := []Directory{
		{Path: "/exact", AllocatedBytes: 100, ApparentBytes: 200},
		{Path: "/small", AllocatedBytes: 10, ApparentBytes: 20},
	}
	if got := tracker.result(10); !reflect.DeepEqual(got, want) || tracker.approximate {
		t.Fatalf("totals before replacement must be exact: got=%+v approximate=%v", got, tracker.approximate)
	}

	// A replacement inherits the evicted counter's bytes. These are estimates,
	// not the bytes actually observed under the replacement path.
	tracker.add("/replacement", 5, 7)
	want[1] = Directory{Path: "/replacement", AllocatedBytes: 15, ApparentBytes: 27, Estimated: true}
	check := func() {
		t.Helper()
		if got := tracker.result(10); !reflect.DeepEqual(got, want) || !tracker.approximate {
			t.Fatalf("estimate evidence lost: got=%+v want=%+v approximate=%v", got, want, tracker.approximate)
		}
		if len(tracker.items) != 2 || len(tracker.byPath) != 2 {
			t.Fatalf("tracker exceeded its bound: heap=%d paths=%d", len(tracker.items), len(tracker.byPath))
		}
	}
	check()
	tracker.add("/replacement", 3, 4)
	want[1].AllocatedBytes, want[1].ApparentBytes = 18, 31
	check()
	tracker.add("/next", 2, 3)
	want[1] = Directory{Path: "/next", AllocatedBytes: 20, ApparentBytes: 34, Estimated: true}
	check()
	// Seeing a previously evicted path again cannot recover an exact total.
	tracker.add("/small", 4, 6)
	want[1] = Directory{Path: "/small", AllocatedBytes: 24, ApparentBytes: 40, Estimated: true}
	check()
	if got := tracker.result(1); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("top limit changed the retained exact row: %+v", got)
	}
	if got := tracker.result(0); got != nil {
		t.Fatalf("zero report limit returned rows: %+v", got)
	}
}

func TestScanReportsEstimatedDirectoriesBeyondTrackerCapacity(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const top = 20
	count := scanCandidateLimit(top) + 1
	for index := 0; index < count; index++ {
		directory := filepath.Join(root, fmt.Sprintf("dir-%04d", index))
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "data"), make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	filesystem, err := FilesystemUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	_, directories, scope, warnings, err := ScanWithScope(root, filesystem.Device, 1, top)
	if err != nil {
		t.Fatal(err)
	}
	if scope.RegularFiles != count || scope.SkippedDepthDirectories != 0 || scope.InspectionErrors != 0 {
		t.Fatalf("tracker capacity must not reduce observed scan coverage: %+v", scope)
	}
	if len(directories) != top {
		t.Fatalf("expected %d reported directories, got %d", top, len(directories))
	}
	estimated := false
	for _, directory := range directories {
		estimated = estimated || directory.Estimated
	}
	if !estimated {
		t.Fatalf("estimated rankings lost their marker: %+v", directories)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "directory totals use bounded heavy-hitter estimates") {
		t.Fatalf("missing directory estimate warning: %v", warnings)
	}
}
