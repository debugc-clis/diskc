package macosystem

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
