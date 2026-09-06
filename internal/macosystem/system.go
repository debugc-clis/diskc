package macosystem

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/debugc-clis/diskc/internal/disk"
)

type Report struct {
	Platform string       `json:"platform"`
	Roots    []RootReport `json:"roots"`
	Warnings []string     `json:"warnings,omitempty"`
}

type RootReport struct {
	Path        string           `json:"path"`
	Category    string           `json:"category"`
	Files       []FileReport     `json:"files,omitempty"`
	Directories []disk.Directory `json:"directories,omitempty"`
	Skipped     bool             `json:"skipped,omitempty"`
	SkipReason  string           `json:"skip_reason,omitempty"`
}

type FileReport struct {
	Path           string  `json:"path"`
	SizeBytes      int64   `json:"size_bytes"`
	Growth         float64 `json:"growth_bytes_per_second,omitempty"`
	Kind           string  `json:"kind"`
	Recommendation string  `json:"recommendation"`
	Risk           string  `json:"risk"`
}

type rootDefinition struct {
	Path     string
	Category string
}

var roots = []rootDefinition{
	{Path: "/private/var", Category: "system state, logs, caches, and temporary data"},
	{Path: "/Library", Category: "system and application support"},
	{Path: "/Users", Category: "user data and application caches"},
	{Path: "/Applications", Category: "installed applications"},
	{Path: "/System", Category: "macOS system files"},
}

func Run(args []string) error {
	flags := flag.NewFlagSet("mac system", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	top := flags.Int("top", 20, "number of large files per system directory")
	depth := flags.Int("depth", 5, "maximum directory depth to scan")
	sample := flags.Duration("sample", 0, "measure growth over this duration, for example 5s")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *top < 1 || *depth < 0 || *sample < 0 {
		return fmt.Errorf("mac system: top must be positive; depth and sample must be non-negative")
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("mac system: this command requires macOS; current platform is %s", runtime.GOOS)
	}
	report := inspect(*top, *depth, *sample)
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	print(report)
	return nil
}

func inspect(top, maxDepth int, sample time.Duration) Report {
	report := Report{Platform: runtime.GOOS}
	for _, definition := range roots {
		if _, err := os.Stat(definition.Path); err != nil {
			continue
		}
		filesystem, err := disk.FilesystemUsage(definition.Path)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("cannot inspect %s: %v", definition.Path, err))
			continue
		}
		files, directories, warnings, err := disk.Scan(definition.Path, filesystem.Device, maxDepth, top)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("cannot scan %s: %v", definition.Path, err))
			continue
		}
		if sample > 0 {
			if err := disk.MeasureGrowth(files, sample); err != nil {
				warnings = append(warnings, err.Error())
			}
		}
		sort.Slice(files, func(i, j int) bool {
			if files[i].Growth == files[j].Growth {
				return files[i].Size > files[j].Size
			}
			return files[i].Growth > files[j].Growth
		})
		if len(files) > top {
			files = files[:top]
		}
		items := make([]FileReport, 0, len(files))
		for _, file := range files {
			recommendation, risk := recommendation(file.Path, file.Size)
			items = append(items, FileReport{Path: file.Path, SizeBytes: file.Size, Growth: file.Growth, Kind: file.Kind, Recommendation: recommendation, Risk: risk})
		}
		report.Roots = append(report.Roots, RootReport{Path: definition.Path, Category: definition.Category, Files: items, Directories: directories})
		report.Warnings = append(report.Warnings, warnings...)
	}
	return report
}

func recommendation(path string, size int64) (string, string) {
	lower := strings.ToLower(filepath.Clean(path))
	switch {
	case strings.HasPrefix(lower, "/system/") || strings.HasPrefix(lower, "/usr/") || strings.HasPrefix(lower, "/bin/") || strings.HasPrefix(lower, "/sbin/"):
		return "Do not delete manually; managed by macOS", "do not delete"
	case strings.HasPrefix(lower, "/private/var/db/") || strings.HasPrefix(lower, "/private/var/vm/"):
		return "Do not delete manually; required by macOS services", "do not delete"
	case strings.Contains(lower, "/caches/") || strings.Contains(lower, "/cache/"):
		return "Usually removable after closing the owning app; re-created when needed", "review before cleaning"
	case strings.Contains(lower, "/logs/") || strings.HasSuffix(lower, ".log"):
		return "Review age and ownership; rotate or remove only known-old logs", "review before cleaning"
	case strings.Contains(lower, "/tmp/") || strings.Contains(lower, "/temporaryitems/"):
		return "Usually temporary; remove only when no related process is running", "review before cleaning"
	case strings.Contains(lower, "/developer/") || strings.Contains(lower, "/xcode/") || strings.Contains(lower, "/deriveddata"):
		return "Review developer tools or Xcode caches; clean through the owning tool", "review before cleaning"
	case strings.Contains(lower, "/application support/") || strings.Contains(lower, "/containers/") || strings.Contains(lower, "/group containers/"):
		return "May contain live application data; use the owning app's cleanup controls", "do not delete manually"
	case strings.Contains(lower, "/library/updates") || strings.Contains(lower, "/updates/"):
		return "Review only after confirming no update is in progress", "review before cleaning"
	case size >= 10<<30:
		return "Inspect contents and owner first; no blanket deletion recommendation", "review before cleaning"
	default:
		return "Inspect contents and owner before removing", "review before cleaning"
	}
}

func print(report Report) {
	fmt.Printf("macOS system disk report\nPlatform: %s\n", report.Platform)
	for _, root := range report.Roots {
		fmt.Printf("\n%s (%s)\n", root.Path, root.Category)
		if len(root.Files) == 0 {
			fmt.Println("  No regular files found at the selected depth.")
			continue
		}
		fmt.Println("  Largest files")
		for _, file := range root.Files {
			growth := ""
			if file.Growth > 0 {
				growth = fmt.Sprintf("  ↑ %s/s", formatBytes(uint64(file.Growth)))
			}
			fmt.Printf("    %10s  %s%s\n", formatBytes(uint64(file.SizeBytes)), file.Path, growth)
			fmt.Printf("      recommendation: %s\n      risk: %s\n", file.Recommendation, file.Risk)
		}
		if len(root.Directories) > 0 {
			fmt.Println("  Largest directories")
			for _, directory := range root.Directories[:min(5, len(root.Directories))] {
				fmt.Printf("    %10s  %s\n", formatBytes(uint64(directory.Size)), directory.Path)
			}
		}
	}
	if len(report.Warnings) > 0 {
		fmt.Println("\nWarnings")
		for _, warning := range report.Warnings {
			fmt.Printf("- %s\n", warning)
		}
	}
	fmt.Println("\nNo files are deleted automatically.")
}

func formatBytes(value uint64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	number := float64(value)
	unit := 0
	for number >= 1024 && unit < len(units)-1 {
		number /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", value)
	}
	return fmt.Sprintf("%.1f %s", number, units[unit])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func unique(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
