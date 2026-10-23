package macosystem

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/debugc-clis/diskc/internal/cliutil"
	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/settings"
	"github.com/debugc-clis/diskc/internal/textfmt"
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
	Growing     []FileReport     `json:"fastest_growing,omitempty"`
	Directories []disk.Directory `json:"directories,omitempty"`
	Skipped     bool             `json:"skipped,omitempty"`
	SkipReason  string           `json:"skip_reason,omitempty"`
	candidates  []disk.File
}

type FileReport struct {
	Path           string  `json:"path"`
	AllocatedBytes int64   `json:"allocated_bytes"`
	ApparentBytes  int64   `json:"apparent_bytes"`
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

func Run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("mac system", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	top := flags.Int("top", settings.Top, "number of large files per system directory")
	depth := flags.Int("depth", settings.MacDepth, "maximum directory depth to scan")
	sample := flags.Duration("sample", 0, "measure growth over this duration, for example 5s")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	flags.Usage = func() {
		fmt.Fprintln(stdout, "Usage: diskc mac system [--top N] [--depth N] [--sample DURATION] [--json]\n\nInspect macOS system storage and provide cautious cleanup guidance.")
		flags.SetOutput(stdout)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return cliutil.Usage(err)
	}
	if flags.NArg() != 0 {
		return cliutil.Usage(fmt.Errorf("mac system: unexpected argument %q", flags.Arg(0)))
	}
	if *top < 1 || *top > disk.MaxReportFiles || *depth < 0 || *sample < 0 {
		return cliutil.Usage(fmt.Errorf("mac system: top must be between 1 and %d; depth and sample must be non-negative", disk.MaxReportFiles))
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("mac system: this command requires macOS; current platform is %s", runtime.GOOS)
	}
	report := inspect(*top, *depth, *sample)
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	return print(stdout, report)
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
		report.Roots = append(report.Roots, RootReport{Path: definition.Path, Category: definition.Category, Directories: directories, candidates: files})
		report.Warnings = append(report.Warnings, warnings...)
	}
	groups := make([][]disk.File, len(report.Roots))
	for index := range report.Roots {
		groups[index] = report.Roots[index].candidates
	}
	if err := disk.MeasureGrowthGroups(groups, sample); err != nil {
		report.Warnings = append(report.Warnings, err.Error())
	}
	for index := range report.Roots {
		largest, growing := disk.SelectReportFiles(report.Roots[index].candidates, top)
		report.Roots[index].Files = fileReports(largest)
		report.Roots[index].Growing = fileReports(growing)
		report.Roots[index].candidates = nil
	}
	return report
}

func fileReports(files []disk.File) []FileReport {
	items := make([]FileReport, 0, len(files))
	for _, file := range files {
		recommendation, risk := recommendation(file.Path, file.AllocatedBytes)
		items = append(items, FileReport{Path: file.Path, AllocatedBytes: file.AllocatedBytes, ApparentBytes: file.ApparentBytes, Growth: file.Growth, Kind: file.Kind, Recommendation: recommendation, Risk: risk})
	}
	return items
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

func print(writer io.Writer, report Report) error {
	checked := textfmt.NewWriter(writer)
	writer = checked
	fmt.Fprintf(writer, "macOS system disk report\nPlatform: %s\n", textfmt.Safe(report.Platform))
	for _, root := range report.Roots {
		fmt.Fprintf(writer, "\n%s (%s)\n", textfmt.Safe(root.Path), textfmt.Safe(root.Category))
		if len(root.Files) == 0 {
			fmt.Fprintln(writer, "  No regular files found at the selected depth.")
			continue
		}
		fmt.Fprintln(writer, "  Largest files")
		for _, file := range root.Files {
			growth := ""
			if file.Growth > 0 {
				growth = fmt.Sprintf("  ↑ %s/s", formatBytes(uint64(file.Growth)))
			}
			fmt.Fprintf(writer, "    %10s  %s%s\n", formatBytes(uint64(file.AllocatedBytes)), textfmt.Safe(file.Path), growth)
			fmt.Fprintf(writer, "      recommendation: %s\n      risk: %s\n", textfmt.Safe(file.Recommendation), textfmt.Safe(file.Risk))
		}
		if len(root.Growing) > 0 {
			fmt.Fprintln(writer, "  Fastest growth")
			for _, file := range root.Growing {
				fmt.Fprintf(writer, "    %10s/s  %s\n", formatBytes(uint64(file.Growth)), textfmt.Safe(file.Path))
			}
		}
		if len(root.Directories) > 0 {
			fmt.Fprintln(writer, "  Largest directories")
			for _, directory := range root.Directories[:min(5, len(root.Directories))] {
				fmt.Fprintf(writer, "    %10s  %s\n", formatBytes(uint64(directory.AllocatedBytes)), textfmt.Safe(directory.Path))
			}
		}
	}
	if len(report.Warnings) > 0 {
		fmt.Fprintln(writer, "\nWarnings")
		for _, warning := range report.Warnings {
			fmt.Fprintf(writer, "- %s\n", textfmt.Safe(warning))
		}
	}
	fmt.Fprintln(writer, "\nNo files are deleted automatically.")
	return checked.Err()
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
