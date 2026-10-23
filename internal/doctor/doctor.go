package doctor

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/debugc-clis/diskc/internal/buildinfo"
	"github.com/debugc-clis/diskc/internal/cliutil"
	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/mount"
	"github.com/debugc-clis/diskc/internal/settings"
	"github.com/debugc-clis/diskc/internal/textfmt"
)

type Defaults struct {
	Top           int    `json:"top"`
	Depth         int    `json:"depth"`
	Sample        string `json:"sample"`
	LightTop      int    `json:"light_top"`
	LightDepth    int    `json:"light_depth"`
	LightSample   string `json:"light_sample"`
	DBTop         int    `json:"db_top"`
	DBDepth       int    `json:"db_depth"`
	DBSample      string `json:"db_sample"`
	DockerTop     int    `json:"docker_top"`
	DockerDepth   int    `json:"docker_depth"`
	DockerSample  string `json:"docker_sample"`
	MacTop        int    `json:"mac_top"`
	MacDepth      int    `json:"mac_depth"`
	MacSample     string `json:"mac_sample"`
	WatchInterval string `json:"watch_interval"`
	All           bool   `json:"all"`
	Watch         bool   `json:"watch"`
	JSON          bool   `json:"json"`
	ProcessChecks bool   `json:"process_checks"`
	DeletedOpen   bool   `json:"deleted_open"`
}

type Check struct {
	Status string `json:"status"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
	Advice string `json:"advice,omitempty"`
}

type Report struct {
	Version    string           `json:"version"`
	Commit     string           `json:"commit"`
	Built      string           `json:"built"`
	GoVersion  string           `json:"go_version"`
	Platform   string           `json:"platform"`
	Path       string           `json:"path"`
	Defaults   Defaults         `json:"defaults"`
	Mount      *mount.Info      `json:"mount,omitempty"`
	Filesystem *disk.Filesystem `json:"filesystem,omitempty"`
	Checks     []Check          `json:"checks"`
}

var listMounts = mount.List
var filesystemUsage = disk.FilesystemUsage

func Run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("doc", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	flags.Usage = func() {
		fmt.Fprintln(stdout, "Usage: diskc doc [path] [--json]\n\nCheck diskc's configuration and filesystem inspection capabilities.")
		flags.SetOutput(stdout)
		flags.PrintDefaults()
	}
	if err := flags.Parse(normalizeArgs(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return cliutil.Usage(err)
	}
	if flags.NArg() > 1 {
		return cliutil.Usage(fmt.Errorf("doc: expected at most one path"))
	}
	target := "/"
	if flags.NArg() == 1 {
		target = flags.Arg(0)
	}
	report := Collect(target)
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	return Print(stdout, report)
}

func Collect(path string) Report {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	report := Report{
		Version:   buildinfo.Version,
		Commit:    buildinfo.Commit,
		Built:     buildinfo.Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Path:      absolute,
		Defaults: Defaults{
			Top: settings.Top, Depth: settings.Depth, Sample: settings.Sample.String(),
			LightTop: settings.LightTop, LightDepth: settings.LightDepth, LightSample: "0s",
			DBTop: settings.Top, DBDepth: settings.Depth, DBSample: settings.Sample.String(),
			DockerTop: settings.Top, DockerDepth: settings.Depth, DockerSample: settings.Sample.String(),
			MacTop: settings.Top, MacDepth: settings.MacDepth, MacSample: "0s",
			WatchInterval: settings.WatchInterval.String(), ProcessChecks: runtime.GOOS == "linux",
			DeletedOpen: runtime.GOOS == "linux",
		},
		Checks: make([]Check, 0, 6),
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		report.Checks = append(report.Checks, Check{Status: "WARNING", Name: "platform", Detail: "this platform is not supported"})
	}
	mounts, mountErr := listMounts()
	if mountErr != nil {
		report.Checks = append(report.Checks, Check{Status: "WARNING", Name: "mount inventory", Detail: mountErr.Error(), Advice: "diskc list and --all may be incomplete"})
	} else {
		report.Checks = append(report.Checks, Check{Status: "OK", Name: "mount inventory", Detail: fmt.Sprintf("%d mounted filesystems discovered", len(mounts))})
		if selected := matchingMount(absolute, mounts); selected != nil {
			report.Mount = selected
			if selected.Kind == mount.KindRemote {
				report.Checks = append(report.Checks, Check{Status: "WARNING", Name: "remote mount", Detail: "filesystem usage was not probed to avoid a possible stale mount", Advice: "inspect this path explicitly only when the mount is responsive"})
			}
			if selected.ReadOnly {
				report.Checks = append(report.Checks, Check{Status: "INFO", Name: "read-only mount", Detail: "writes are unavailable on this mount"})
			}
		}
	}
	if report.Mount == nil || report.Mount.Kind != mount.KindRemote {
		usage, usageErr := filesystemUsage(absolute)
		if usageErr != nil {
			report.Checks = append(report.Checks, Check{Status: "WARNING", Name: "filesystem access", Detail: usageErr.Error(), Advice: "check the path and the current user's permissions"})
		} else {
			report.Filesystem = &usage
			report.Checks = append(report.Checks, usageChecks(usage)...)
		}
	}
	if runtime.GOOS == "linux" {
		if _, err := os.ReadDir("/proc/self/fd"); err != nil {
			report.Checks = append(report.Checks, Check{Status: "WARNING", Name: "process inspection", Detail: "/proc/self/fd is unavailable", Advice: "writer and deleted-open-file details may be missing"})
		} else {
			report.Checks = append(report.Checks, Check{Status: "OK", Name: "process inspection", Detail: "/proc is readable; other users' processes may still be hidden by permissions"})
		}
	} else {
		report.Checks = append(report.Checks, Check{Status: "INFO", Name: "process inspection", Detail: "Linux /proc writer attribution is not available on this platform"})
	}
	return report
}

func normalizeArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	paths := make([]string, 0, 1)
	for index, arg := range args {
		if arg == "--" {
			paths = append(paths, args[index+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
		} else {
			paths = append(paths, arg)
		}
	}
	return append(flags, paths...)
}

func matchingMount(path string, mounts []mount.Info) *mount.Info {
	var selected *mount.Info
	for index := range mounts {
		candidate := &mounts[index]
		if path != candidate.Path && !strings.HasPrefix(path, strings.TrimRight(candidate.Path, "/")+"/") {
			continue
		}
		if selected == nil || len(candidate.Path) > len(selected.Path) {
			selected = candidate
		}
	}
	if selected == nil {
		return nil
	}
	copy := *selected
	return &copy
}

func usageChecks(usage disk.Filesystem) []Check {
	checks := make([]Check, 0, 3)
	pressureStatus := "OK"
	pressureAdvice := ""
	if usage.PressurePercent >= 95 {
		pressureStatus = "CRITICAL"
		pressureAdvice = "investigate large and growing files with diskc on this path"
	} else if usage.PressurePercent >= 90 {
		pressureStatus = "WARNING"
		pressureAdvice = "investigate large and growing files with diskc on this path"
	}
	checks = append(checks, Check{Status: pressureStatus, Name: "capacity", Detail: fmt.Sprintf("%.1f%% unavailable; %s available", usage.PressurePercent, formatBytes(usage.AvailableBytes)), Advice: pressureAdvice})
	inodeStatus := "OK"
	inodeAdvice := ""
	if usage.InodePercent >= 95 {
		inodeStatus = "CRITICAL"
		inodeAdvice = "inspect directories with many small files"
	} else if usage.InodePercent >= 90 {
		inodeStatus = "WARNING"
		inodeAdvice = "inspect directories with many small files"
	}
	checks = append(checks, Check{Status: inodeStatus, Name: "inodes", Detail: fmt.Sprintf("%.1f%% used", usage.InodePercent), Advice: inodeAdvice})
	return checks
}

func formatBytes(value uint64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	amount := float64(value)
	unit := 0
	for amount >= 1024 && unit < len(units)-1 {
		amount /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", value)
	}
	return fmt.Sprintf("%.1f %s", amount, units[unit])
}

func Print(writer io.Writer, report Report) error {
	checked := textfmt.NewWriter(writer)
	fmt.Fprintln(checked, "diskc doctor")
	fmt.Fprintf(checked, "Build: %s (%s)  Go: %s  Platform: %s\n", textfmt.Safe(report.Version), textfmt.Safe(report.Commit), textfmt.Safe(report.GoVersion), textfmt.Safe(report.Platform))
	fmt.Fprintf(checked, "Built: %s\nTarget: %s\n", textfmt.Safe(report.Built), textfmt.Safe(report.Path))
	fmt.Fprintln(checked, "\nConfiguration (built-in defaults; no configuration file)")
	fmt.Fprintf(checked, "  full: top=%d depth=%d sample=%s\n", report.Defaults.Top, report.Defaults.Depth, report.Defaults.Sample)
	fmt.Fprintf(checked, "  light: top=%d depth=%d sample=%s\n", report.Defaults.LightTop, report.Defaults.LightDepth, report.Defaults.LightSample)
	fmt.Fprintf(checked, "  db: top=%d depth=%d sample=%s\n", report.Defaults.DBTop, report.Defaults.DBDepth, report.Defaults.DBSample)
	fmt.Fprintf(checked, "  docker: top=%d depth=%d sample=%s\n", report.Defaults.DockerTop, report.Defaults.DockerDepth, report.Defaults.DockerSample)
	fmt.Fprintf(checked, "  mac system: top=%d depth=%d sample=%s\n", report.Defaults.MacTop, report.Defaults.MacDepth, report.Defaults.MacSample)
	fmt.Fprintf(checked, "  all mounts: %t  watch: %t  json: %t\n", report.Defaults.All, report.Defaults.Watch, report.Defaults.JSON)
	fmt.Fprintf(checked, "  watch interval: %s\n  Linux process checks: %t  deleted-open checks: %t\n", report.Defaults.WatchInterval, report.Defaults.ProcessChecks, report.Defaults.DeletedOpen)
	if report.Mount != nil {
		fmt.Fprintf(checked, "\nMount: %s  source=%s  type=%s  kind=%s\n", textfmt.Safe(report.Mount.Path), textfmt.Safe(report.Mount.Source), textfmt.Safe(report.Mount.Type), textfmt.Safe(report.Mount.Kind))
	}
	fmt.Fprintln(checked, "\nChecks")
	issues := 0
	for _, check := range report.Checks {
		fmt.Fprintf(checked, "  [%s] %s: %s\n", check.Status, check.Name, textfmt.Safe(check.Detail))
		if check.Advice != "" {
			fmt.Fprintf(checked, "       next: %s\n", textfmt.Safe(check.Advice))
		}
		if check.Status == "WARNING" || check.Status == "CRITICAL" {
			issues++
		}
	}
	if issues == 0 {
		fmt.Fprintln(checked, "\nNo problems found in the checks performed.")
	} else {
		fmt.Fprintf(checked, "\n%d issue(s) found.\n", issues)
	}
	return checked.Err()
}
