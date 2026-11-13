package macosystem

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/debugc-clis/diskc/internal/cliutil"
	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/settings"
)

// RunUser inspects selected storage directories in the current user's home.
func RunUser(args []string, stdout, stderr io.Writer) error {
	return runUser(args, stdout, runtime.GOOS, os.UserHomeDir, rootOperations{
		stat: os.Stat, filesystemUsage: disk.FilesystemUsage, scan: disk.ScanWithScope,
	})
}

func runUser(args []string, stdout io.Writer, platform string, homeDir func() (string, error), operations rootOperations) error {
	flags := flag.NewFlagSet("mac user", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	top := flags.Int("top", settings.Top, "number of large files per user directory")
	depth := flags.Int("depth", settings.MacDepth, "maximum directory depth to scan")
	sample := flags.Duration("sample", 0, "measure growth over this duration, for example 5s")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	flags.Usage = func() {
		fmt.Fprintln(stdout, "Usage: diskc mac user [--top N] [--depth N] [--sample DURATION] [--json]\n\nInspect selected macOS user storage directories without modifying files.")
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
		return cliutil.Usage(fmt.Errorf("mac user: unexpected argument %q", flags.Arg(0)))
	}
	if *top < 1 || *top > disk.MaxReportFiles || *depth < 0 || *sample < 0 {
		return cliutil.Usage(fmt.Errorf("mac user: top must be between 1 and %d; depth and sample must be non-negative", disk.MaxReportFiles))
	}
	if platform != "darwin" {
		return fmt.Errorf("mac user: this command requires macOS; current platform is %s", platform)
	}
	home, err := homeDir()
	if err != nil {
		return fmt.Errorf("mac user: cannot determine current user's home: %w", err)
	}
	if home == "" || !filepath.IsAbs(home) {
		return fmt.Errorf("mac user: current user's home must be a non-empty absolute path")
	}
	report := inspectUser(filepath.Clean(home), *top, *depth, *sample, operations)
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	return print(stdout, report)
}

func userRoots(home string) []rootDefinition {
	return []rootDefinition{
		{Path: filepath.Join(home, "Library", "Caches"), Category: "application caches"},
		{Path: filepath.Join(home, "Library", "Logs"), Category: "application logs"},
		{Path: filepath.Join(home, "Library", "Application Support"), Category: "application support and live data"},
		{Path: filepath.Join(home, "Library", "Containers"), Category: "sandboxed application data"},
		{Path: filepath.Join(home, "Library", "Group Containers"), Category: "shared application data"},
		{Path: filepath.Join(home, "Library", "Developer"), Category: "developer tools and data"},
		{Path: filepath.Join(home, "Downloads"), Category: "downloaded and personal files"},
		{Path: filepath.Join(home, ".Trash"), Category: "recoverable files in Trash"},
	}
}

func inspectUser(home string, top, maxDepth int, sample time.Duration, operations rootOperations) Report {
	report := inspectRoots(userRoots(home), top, maxDepth, sample, operations)
	report.Scope = "user"
	report.Home = home
	for index := range report.Roots {
		root := &report.Roots[index]
		relative, _ := filepath.Rel(home, root.Path)
		for _, files := range [][]FileReport{root.Files, root.Growing} {
			for fileIndex := range files {
				files[fileIndex].Recommendation, files[fileIndex].Risk = userRecommendation(relative)
			}
		}
	}
	return report
}

// Classify by the selected root, before any filename hints. Cache/log names
// nested in application data must never weaken its protection.
func userRecommendation(root string) (string, string) {
	switch root {
	case filepath.Join("Library", "Application Support"), filepath.Join("Library", "Containers"), filepath.Join("Library", "Group Containers"):
		return "May contain live application data; use the owning app's storage controls", "do not delete manually"
	case filepath.Join("Library", "Developer"):
		return "May contain archives, simulators, and other developer data; inspect through the owning developer tool", "review before cleaning"
	case filepath.Join("Library", "Caches"):
		return "Review contents and owning app; cache files may be in use or linked to other data", "review before cleaning"
	case filepath.Join("Library", "Logs"):
		return "Review age and owning app; use its log rotation controls", "review before cleaning"
	case "Downloads":
		return "May contain unique personal files; review contents before removing", "review before cleaning"
	case ".Trash":
		return "May contain recoverable personal files; review in Finder before emptying Trash", "review before cleaning"
	default:
		return "Inspect contents and owner before removing", "review before cleaning"
	}
}
