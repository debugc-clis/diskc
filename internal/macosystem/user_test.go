package macosystem

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/debugc-clis/diskc/internal/disk"
)

func userTestOperations() rootOperations {
	return rootOperations{stat: os.Stat, filesystemUsage: disk.FilesystemUsage, scan: disk.ScanWithScope}
}

func TestInspectUserSelectedRootsAndProtectiveGuidance(t *testing.T) {
	home := filepath.Join(t.TempDir(), "User With Spaces")
	definitions := userRoots(home)
	if len(definitions) != 8 {
		t.Fatalf("unexpected root definitions: %+v", definitions)
	}
	for _, definition := range definitions {
		path := filepath.Join(definition.Path, "App", "Caches", "example.log")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 8192), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, "Desktop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "Desktop", "unselected"), make([]byte, 16384), 0o600); err != nil {
		t.Fatal(err)
	}
	report := inspectUser(home, 1, 2, time.Millisecond, userTestOperations())
	if report.Scope != "user" || report.Home != home || len(report.Roots) != len(definitions) {
		t.Fatalf("unexpected user scope: %+v", report)
	}
	seen := make(map[string]bool)
	for index, root := range report.Roots {
		if root.Path != definitions[index].Path || seen[root.Path] || root.Skipped || len(root.Files) != 1 || root.ScanScope == nil || root.ScanScope.RegularFiles != 1 {
			t.Fatalf("selected root grouping lost: %+v", root)
		}
		seen[root.Path] = true
		file := root.Files[0]
		if file.AllocatedBytes <= 0 || file.ApparentBytes != 8192 || file.GrowthStatus != "measured" {
			t.Fatalf("file evidence or sampling lost: %+v", file)
		}
		relative, err := filepath.Rel(home, root.Path)
		if err != nil {
			t.Fatal(err)
		}
		switch relative {
		case filepath.Join("Library", "Application Support"), filepath.Join("Library", "Containers"), filepath.Join("Library", "Group Containers"):
			if file.Risk != "do not delete manually" || !strings.Contains(file.Recommendation, "live application data") {
				t.Fatalf("nested cache/log hints weakened application data protection: %+v", file)
			}
		case filepath.Join("Library", "Developer"):
			if !strings.Contains(file.Recommendation, "archives, simulators") || strings.Contains(file.Recommendation, "removable") {
				t.Fatalf("developer data treated as disposable cache: %+v", file)
			}
		case filepath.Join("Library", "Caches"):
			if !strings.Contains(file.Recommendation, "linked to other data") || strings.Contains(file.Recommendation, "removable") {
				t.Fatalf("cache recommendation is too broad: %+v", file)
			}
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("unselected")) || bytes.Contains(encoded, []byte(`"usage"`)) {
		t.Fatalf("user report claimed unrelated home data or duplicated filesystem capacity: %s", encoded)
	}
}

func TestInspectUserMissingPartialAndUnavailableRoots(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	info, err := os.Stat(filepath.Dir(home))
	if err != nil {
		t.Fatal(err)
	}
	definitions := userRoots(home)
	operations := rootOperations{
		stat: func(path string) (os.FileInfo, error) {
			switch path {
			case definitions[0].Path, definitions[2].Path, definitions[3].Path:
				return info, nil
			case definitions[1].Path:
				return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrPermission}
			default:
				return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
			}
		},
		filesystemUsage: func(path string) (disk.Filesystem, error) {
			if path == definitions[2].Path {
				return disk.Filesystem{}, errors.New("filesystem unavailable")
			}
			return disk.Filesystem{Device: 1}, nil
		},
		scan: func(path string, _ uint64, depth, _ int) ([]disk.File, []disk.Directory, disk.ScanScope, []string, error) {
			if path == definitions[3].Path {
				return nil, nil, disk.ScanScope{}, []string{"scan detail"}, errors.New("fatal scan")
			}
			return nil, nil, disk.ScanScope{Root: path, MaxDepth: depth, InspectionErrors: 1}, []string{"partial traversal"}, nil
		},
	}
	report := inspectUser(home, 1, 3, 0, operations)
	if len(report.Roots) != 4 || len(report.Warnings) != 5 || report.Roots[0].ScanScope == nil || report.Roots[0].ScanScope.InspectionErrors != 1 {
		t.Fatalf("partial/missing roots misreported: %+v", report)
	}
	for _, root := range report.Roots[1:] {
		if !root.Skipped || root.SkipReason == "" || root.ScanScope != nil || len(root.Files) != 0 {
			t.Fatalf("unavailable root claimed successful evidence: %+v", root)
		}
	}
	var output bytes.Buffer
	if err := print(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"macOS user disk report", "selected home directories only", "APFS snapshots", "do not add", "No regular files observed; inspection errors may hide files.", "Skipped:", "partial traversal", "scan detail", "fatal scan"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("user text missing %q: %s", expected, output.String())
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Scope != "user" || decoded.Home != home || len(decoded.Roots) != 4 || !decoded.Roots[1].Skipped || decoded.Roots[0].ScanScope.InspectionErrors != 1 {
		t.Fatalf("user evidence lost in JSON: %s", encoded)
	}
}

func TestRunUserHelpValidationAndPlatform(t *testing.T) {
	for _, args := range [][]string{{"--top", "0"}, {"--top", "10001"}, {"--depth", "-1"}, {"--sample", "-1s"}, {"--unknown"}, {"extra"}} {
		if err := runUser(args, &bytes.Buffer{}, "darwin", func() (string, error) {
			t.Fatal("invalid arguments reached home discovery")
			return "", nil
		}, rootOperations{}); err == nil {
			t.Errorf("expected usage error for %v", args)
		}
	}
	var output bytes.Buffer
	if err := runUser([]string{"--help"}, &output, "linux", nil, rootOperations{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Usage: diskc mac user") || !strings.Contains(output.String(), "default 5") || !strings.Contains(output.String(), "default 20") {
		t.Fatalf("user help missing defaults: %s", output.String())
	}
	if err := runUser(nil, &bytes.Buffer{}, "linux", func() (string, error) {
		t.Fatal("non-mac invocation reached home discovery")
		return "", nil
	}, rootOperations{}); err == nil || !strings.Contains(err.Error(), "requires macOS") {
		t.Fatalf("non-mac invocation was not rejected: %v", err)
	}
}

func TestRunUserRejectsUnavailableHome(t *testing.T) {
	for _, test := range []struct {
		home string
		err  error
	}{
		{err: errors.New("home lookup failed")},
		{},
		{home: "relative/home"},
	} {
		err := runUser(nil, &bytes.Buffer{}, "darwin", func() (string, error) { return test.home, test.err }, rootOperations{})
		if err == nil || !strings.Contains(err.Error(), "home") {
			t.Fatalf("invalid home %q accepted: %v", test.home, err)
		}
	}
}

func TestRunUserJSONAndFlags(t *testing.T) {
	home := filepath.Join(t.TempDir(), "User With Spaces")
	root := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(filepath.Join(root, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "child", "hidden")} {
		if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := runUser([]string{"--json", "--top", "1", "--depth", "0", "--sample", "0"}, &output, "darwin", func() (string, error) { return home, nil }, userTestOperations()); err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Scope != "user" || report.Home != home || len(report.Roots) != 1 || len(report.Roots[0].Files) != 1 || report.Roots[0].ScanScope.MaxDepth != 0 || report.Roots[0].ScanScope.RegularFiles != 2 || report.Roots[0].ScanScope.SkippedDepthDirectories != 1 {
		t.Fatalf("user CLI flags or selected scope lost: %s", output.String())
	}
}

func TestUserGuidancePreservesGrowingAndEstimatedEvidence(t *testing.T) {
	home := t.TempDir()
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "Library", "Containers")
	operations := rootOperations{
		stat: func(path string) (os.FileInfo, error) {
			if path == root {
				return info, nil
			}
			return nil, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
		},
		filesystemUsage: func(string) (disk.Filesystem, error) { return disk.Filesystem{Device: 1}, nil },
		scan: func(path string, _ uint64, depth, _ int) ([]disk.File, []disk.Directory, disk.ScanScope, []string, error) {
			return []disk.File{{Path: filepath.Join(path, "App", "Caches", "active.log"), AllocatedBytes: 4096, ApparentBytes: 8192, Growth: 1024, GrowthStatus: "measured"}},
				[]disk.Directory{{Path: filepath.Join(path, "App"), AllocatedBytes: 4096, ApparentBytes: 8192, Estimated: true}},
				disk.ScanScope{Root: path, MaxDepth: depth, RegularFiles: 1}, nil, nil
		},
	}
	report := inspectUser(home, 1, 2, 0, operations)
	if len(report.Roots) != 1 || len(report.Roots[0].Files) != 1 || len(report.Roots[0].Growing) != 1 || len(report.Roots[0].Directories) != 1 || !report.Roots[0].Directories[0].Estimated {
		t.Fatalf("file/growth/directory evidence lost: %+v", report)
	}
	for _, file := range []FileReport{report.Roots[0].Files[0], report.Roots[0].Growing[0]} {
		if file.Risk != "do not delete manually" || file.AllocatedBytes != 4096 || file.ApparentBytes != 8192 || file.Growth != 1024 || file.GrowthStatus != "measured" {
			t.Fatalf("user guidance changed evidence or weakened data protection: %+v", file)
		}
	}
	var output bytes.Buffer
	if err := print(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"[8.0 KB apparent]", "Fastest growth", "(estimated)", "do not delete manually"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("user text lost %q: %s", expected, output.String())
		}
	}
}

func TestUserHeaderEscapingAndSystemCompatibility(t *testing.T) {
	home := "/Users/operator\n\x1b[31m"
	var output bytes.Buffer
	if err := print(&output, Report{Platform: "darwin", Scope: "user", Home: home}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `Home: /Users/operator\n\x1b[31m`) || strings.ContainsRune(output.String(), '\x1b') {
		t.Fatalf("unsafe user home in text: %q", output.String())
	}
	encoded, err := json.Marshal(Report{Platform: "darwin", Scope: "user", Home: home})
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Home != home {
		t.Fatalf("raw home changed in JSON: %s / %v", encoded, err)
	}
	encoded, err = json.Marshal(Report{Platform: "darwin"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"scope"`)) || bytes.Contains(encoded, []byte(`"home"`)) {
		t.Fatalf("system JSON gained user fields: %s", encoded)
	}
	output.Reset()
	if err := print(&output, Report{Platform: "darwin"}); err != nil || !strings.HasPrefix(output.String(), "macOS system disk report\nPlatform: darwin\n") || strings.Contains(output.String(), "Home:") {
		t.Fatalf("system output changed: %s / %v", output.String(), err)
	}
}
