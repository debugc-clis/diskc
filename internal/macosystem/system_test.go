package macosystem

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debugc-clis/diskc/internal/disk"
)

func TestRecommendationProtectsSystemFiles(t *testing.T) {
	recommendation, risk := recommendation("/System/Library/CoreServices/example", 20<<30)
	if risk != "do not delete" || recommendation == "" {
		t.Fatalf("unexpected system recommendation: %q / %q", recommendation, risk)
	}
}

func TestRecommendationClassifiesCaches(t *testing.T) {
	recommendation, risk := recommendation("/Users/test/Library/Caches/example/cache.db", 12<<30)
	if risk != "review before cleaning" || recommendation == "" {
		t.Fatalf("unexpected cache recommendation: %q / %q", recommendation, risk)
	}
}

func TestRecommendationProtectsApplicationData(t *testing.T) {
	recommendation, risk := recommendation("/Users/test/Library/Application Support/App/database.sqlite", 12<<30)
	if risk != "do not delete manually" || recommendation == "" {
		t.Fatalf("unexpected application data recommendation: %q / %q", recommendation, risk)
	}
}

func TestInspectAndPrintTemporaryRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large.log"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "small.log"), make([]byte, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	oldRoots := roots
	roots = []rootDefinition{{Path: root, Category: "test storage"}}
	t.Cleanup(func() { roots = oldRoots })
	report := inspect(1, 2, 0)
	if len(report.Roots) != 1 || len(report.Roots[0].Files) != 1 || report.Roots[0].Files[0].AllocatedBytes <= 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	var output bytes.Buffer
	if err := print(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"macOS system disk report", "Largest files", "No files are deleted automatically"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q in %s", expected, output.String())
		}
	}
}

func TestFileReportsPreserveUnmeasuredGrowth(t *testing.T) {
	files := fileReports([]disk.File{
		{Path: "/private/var/replaced.log", AllocatedBytes: 4096, GrowthStatus: "replaced"},
		{Path: "/private/var/missing.log", AllocatedBytes: 8192, GrowthStatus: "unavailable"},
	})
	report := Report{Platform: "darwin", Roots: []RootReport{{Path: "/private/var", Files: files}}}
	var output bytes.Buffer
	if err := print(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"file replaced; resample", "file unavailable or no longer regular; last observed size"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q in output: %s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), "Fastest growth") || strings.Contains(output.String(), "↑") {
		t.Fatalf("unmeasured growth presented as growing: %s", output.String())
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var got Report
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if files := got.Roots[0].Files; files[0].GrowthStatus != "replaced" || files[1].GrowthStatus != "unavailable" {
		t.Fatalf("growth status lost in JSON: %+v", files)
	}
}

func TestPrintDirectoryEstimatesAndJSON(t *testing.T) {
	report := Report{Platform: "darwin", Roots: []RootReport{{Path: "/private/var", Files: []FileReport{
		{Path: "/private/var/app.log", AllocatedBytes: 4096},
	}, Directories: []disk.Directory{
		{Path: "/private/var/estimated\n\x1b[31m", AllocatedBytes: 8192, ApparentBytes: 8192, Estimated: true},
		{Path: "/private/var/exact", AllocatedBytes: 4096, ApparentBytes: 4096},
	}}}}
	var output bytes.Buffer
	if err := print(&output, report); err != nil {
		t.Fatal(err)
	}
	estimatedRow := `8.0 KB  /private/var/estimated\n\x1b[31m  (estimated)` + "\n"
	exactRow := "4.0 KB  /private/var/exact\n"
	if !strings.Contains(output.String(), estimatedRow) || !strings.Contains(output.String(), exactRow) || strings.Count(output.String(), "(estimated)") != 1 {
		t.Fatalf("directory estimate labels incorrect: %s", output.String())
	}
	if strings.Index(output.String(), estimatedRow) > strings.Index(output.String(), exactRow) || strings.ContainsAny(output.String(), "\x1b\r") {
		t.Fatalf("directory order or terminal escaping changed: %q", output.String())
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Roots []struct {
			Directories []map[string]json.RawMessage `json:"directories"`
		} `json:"roots"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Roots) != 1 || len(decoded.Roots[0].Directories) != 2 {
		t.Fatalf("directory evidence missing from JSON: %s", encoded)
	}
	directories := decoded.Roots[0].Directories
	if string(directories[0]["estimated"]) != "true" {
		t.Fatalf("estimated directory lost its JSON flag: %s", encoded)
	}
	if _, exists := directories[1]["estimated"]; exists {
		t.Fatalf("exact directory unexpectedly has a JSON estimate flag: %s", encoded)
	}
}

func TestInspectEmptyRootsPreservesDistinctScopeInTextAndJSON(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	empty := filepath.Join(base, "empty")
	if err := os.MkdirAll(filepath.Join(root, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child", "data"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	oldRoots := roots
	t.Cleanup(func() { roots = oldRoots })
	roots = []rootDefinition{{Path: root}, {Path: empty}}
	report := inspect(1, 0, 0)
	if len(report.Roots) != 2 || report.Roots[0].ScanScope == nil || report.Roots[1].ScanScope == nil || report.Roots[0].ScanScope == report.Roots[1].ScanScope {
		t.Fatalf("per-root scopes missing or shared: %+v", report)
	}
	if report.Roots[0].ScanScope.RegularFiles != 0 || report.Roots[0].ScanScope.MaxDepth != 0 || report.Roots[0].ScanScope.SkippedDepthDirectories != 1 || report.Roots[1].ScanScope.SkippedDepthDirectories != 0 {
		t.Fatalf("scope incorrect: %+v / %+v", report.Roots[0].ScanScope, report.Roots[1].ScanScope)
	}
	var output bytes.Buffer
	if err := print(&output, report); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "Scan scope:") != 2 || !strings.Contains(output.String(), "increase --depth") || strings.Index(output.String(), "Scan scope:") > strings.Index(output.String(), "No regular files") {
		t.Fatalf("empty scopes lost: %s", output.String())
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Roots[0].ScanScope == nil || *decoded.Roots[0].ScanScope != *report.Roots[0].ScanScope || decoded.Roots[1].ScanScope == nil || *decoded.Roots[1].ScanScope != *report.Roots[1].ScanScope {
		t.Fatalf("scope lost in JSON: %s", encoded)
	}
}

func TestPrintApparentSizesWithExistingEvidence(t *testing.T) {
	files := []disk.File{
		{Path: "/data/sparse\n\x1b[31m", AllocatedBytes: 8192, ApparentBytes: 1 << 30, Growth: 1024},
		{Path: "/data/small", AllocatedBytes: 4096, ApparentBytes: 1, GrowthStatus: "replaced"},
		{Path: "/data/equal", AllocatedBytes: 2048, ApparentBytes: 2048},
		{Path: "/data/zero", AllocatedBytes: 1024, GrowthStatus: "unavailable"},
	}
	directories := []disk.Directory{
		{Path: "/data/sparse-dir\n\x1b[31m", AllocatedBytes: 8192, ApparentBytes: 1 << 30, Estimated: true},
		{Path: "/data/small-dir", AllocatedBytes: 4096, ApparentBytes: 1},
		{Path: "/data/equal-dir", AllocatedBytes: 2048, ApparentBytes: 2048},
		{Path: "/data/zero-dir", AllocatedBytes: 1024},
	}
	report := Report{Platform: "darwin", Roots: []RootReport{{Path: "/data", Files: fileReports(files), Directories: directories}}}
	var output bytes.Buffer
	if err := print(&output, report); err != nil {
		t.Fatal(err)
	}
	expectedRows := []string{
		`8.0 KB  /data/sparse\n\x1b[31m  [1.0 GB apparent]  ↑ 1.0 KB/s`,
		"4.0 KB  /data/small  [1 B apparent]  [growth unmeasured: file replaced; resample]",
		"2.0 KB  /data/equal",
		"1.0 KB  /data/zero  [growth unmeasured: file unavailable or no longer regular; last observed size]",
		`8.0 KB  /data/sparse-dir\n\x1b[31m  [1.0 GB apparent]  (estimated)`,
		"4.0 KB  /data/small-dir  [1 B apparent]",
		"2.0 KB  /data/equal-dir",
		"1.0 KB  /data/zero-dir",
	}
	previous := -1
	for _, row := range expectedRows {
		index := strings.Index(output.String(), row+"\n")
		if index < 0 {
			t.Fatalf("missing complete row %q in output: %s", row, output.String())
		}
		if index <= previous {
			t.Fatalf("row order changed: %s", output.String())
		}
		previous = index
	}
	if strings.Count(output.String(), " apparent]") != 4 || strings.ContainsAny(output.String(), "\x1b\r") {
		t.Fatalf("apparent-size omission or terminal escaping changed: %q", output.String())
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	gotFiles, gotDirectories := decoded.Roots[0].Files, decoded.Roots[0].Directories
	for index, file := range gotFiles {
		if file.Path != files[index].Path || file.AllocatedBytes != files[index].AllocatedBytes || file.ApparentBytes != files[index].ApparentBytes || file.Growth != files[index].Growth || file.GrowthStatus != files[index].GrowthStatus {
			t.Fatalf("file evidence changed in JSON: %+v; want %+v", file, files[index])
		}
	}
	for index, directory := range gotDirectories {
		if directory != directories[index] {
			t.Fatalf("directory evidence changed in JSON: %+v; want %+v", directory, directories[index])
		}
	}
}

func TestSparseFileScanPreservesAllocatedRankingAndApparentSize(t *testing.T) {
	root := t.TempDir()
	sparsePath := filepath.Join(root, "sparse.img")
	sparse, err := os.Create(sparsePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(1 << 30); err != nil {
		_ = sparse.Close()
		t.Fatal(err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "allocated.bin"), bytes.Repeat([]byte{1}, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	oldRoots := roots
	t.Cleanup(func() { roots = oldRoots })
	roots = []rootDefinition{{Path: root, Category: "test storage"}}
	report := inspect(2, 1, 0)
	if len(report.Roots) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Roots[0].Files) != 2 || decoded.Roots[0].Files[1].ApparentBytes != 1<<30 {
		t.Fatalf("sparse size lost in JSON: %s", encoded)
	}
	files := report.Roots[0].Files
	if len(files) != 2 || files[0].Path != filepath.Join(resolvedRoot, "allocated.bin") || files[1].Path != filepath.Join(resolvedRoot, "sparse.img") {
		t.Fatalf("files not ranked by allocated bytes: %+v", files)
	}
	if files[1].ApparentBytes != 1<<30 || files[1].AllocatedBytes >= files[0].AllocatedBytes || files[1].AllocatedBytes >= files[1].ApparentBytes {
		t.Fatalf("real sparse-file size evidence lost: %+v", files)
	}
	var output bytes.Buffer
	if err := print(&output, report); err != nil {
		t.Fatal(err)
	}
	sparseRow := formatBytes(uint64(files[1].AllocatedBytes)) + "  " + filepath.Join(resolvedRoot, "sparse.img") + "  [1.0 GB apparent]"
	if !strings.Contains(output.String(), sparseRow+"\n") || strings.Index(output.String(), filepath.Join(resolvedRoot, "allocated.bin")) > strings.Index(output.String(), sparseRow) {
		t.Fatalf("sparse-file annotation or allocated order missing: %s", output.String())
	}
}
