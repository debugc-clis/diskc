package docker

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/debugc-clis/diskc/internal/cliutil"
	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/proc"
	"github.com/debugc-clis/diskc/internal/settings"
	"github.com/debugc-clis/diskc/internal/textfmt"
)

type Report struct {
	Runtime   string       `json:"runtime"`
	Rootless  bool         `json:"rootless"`
	Config    string       `json:"config,omitempty"`
	DataRoot  string       `json:"data_root,omitempty"`
	Socket    string       `json:"socket,omitempty"`
	Processes []Process    `json:"processes,omitempty"`
	Paths     []PathReport `json:"paths,omitempty"`
	Warnings  []string     `json:"warnings,omitempty"`
}

type Process struct {
	PID     int    `json:"pid"`
	Process string `json:"process"`
	Exe     string `json:"exe,omitempty"`
}

type PathReport struct {
	Path        string           `json:"path"`
	Kind        string           `json:"kind"`
	Usage       disk.Filesystem  `json:"usage"`
	Files       []disk.File      `json:"files,omitempty"`
	Growing     []disk.File      `json:"fastest_growing,omitempty"`
	Directories []disk.Directory `json:"directories,omitempty"`
	Warnings    []string         `json:"warnings,omitempty"`
	Error       string           `json:"error,omitempty"`
	candidates  []disk.File
}

type daemonConfig struct {
	DataRoot string `json:"data-root"`
}

var discover = Discover
var filesystemUsage = disk.FilesystemUsage
var scan = disk.Scan
var measureGrowthGroups = disk.MeasureGrowthGroupsWithFilesystems
var attributeWriters = proc.AttributeWriters

func Run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("docker", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	top := flags.Int("top", settings.Top, "number of large Docker files to report per path")
	depth := flags.Int("depth", settings.Depth, "maximum Docker directory depth to scan")
	sample := flags.Duration("sample", settings.Sample, "measure Docker file growth over this duration")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	flags.Usage = func() {
		fmt.Fprintln(stdout, "Usage: diskc docker [--top N] [--depth N] [--sample DURATION] [--json]\n\nInspect Docker host storage without requiring daemon access.")
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
		return cliutil.Usage(fmt.Errorf("docker: unexpected argument %q", flags.Arg(0)))
	}
	if *top < 1 || *top > disk.MaxReportFiles || *depth < 0 || *sample < 0 {
		return cliutil.Usage(fmt.Errorf("docker: top must be between 1 and %d; depth and sample must be non-negative", disk.MaxReportFiles))
	}
	report := discover()
	for index := range report.Paths {
		path := &report.Paths[index]
		usage, err := filesystemUsage(path.Path)
		if err != nil {
			path.Error = fmt.Sprintf("cannot inspect: %v", err)
			continue
		}
		path.Usage = usage
		files, directories, warnings, err := scan(path.Path, usage.Device, *depth, *top)
		path.Warnings = append(path.Warnings, warnings...)
		if err != nil {
			path.Error = fmt.Sprintf("cannot scan: %v", err)
			continue
		}
		path.candidates = files
		path.Directories = directories
	}
	groups := make([][]disk.File, 0, len(report.Paths))
	filesystems := make([]*disk.Filesystem, 0, len(report.Paths))
	for index := range report.Paths {
		if report.Paths[index].Error == "" && report.Paths[index].candidates != nil {
			groups = append(groups, report.Paths[index].candidates)
			filesystems = append(filesystems, &report.Paths[index].Usage)
		}
	}
	if err := measureGrowthGroups(groups, filesystems, *sample); err != nil {
		for index := range report.Paths {
			if report.Paths[index].Error == "" {
				report.Paths[index].Warnings = append(report.Paths[index].Warnings, err.Error())
			}
		}
	}
	candidates := make([]disk.File, 0)
	for index := range report.Paths {
		path := &report.Paths[index]
		if path.Error != "" {
			continue
		}
		path.Files, path.Growing = disk.SelectReportFiles(path.candidates, *top)
		candidates = append(candidates, path.Files...)
		candidates = append(candidates, path.Growing...)
	}
	attributeWriters(candidates)
	writers := make(map[string][]disk.Writer, len(candidates))
	for _, file := range candidates {
		if len(file.Writers) > 0 {
			writers[file.Path] = file.Writers
		}
	}
	for index := range report.Paths {
		attachWriters(report.Paths[index].Files, writers)
		attachWriters(report.Paths[index].Growing, writers)
		report.Paths[index].candidates = nil
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	return printReport(stdout, report)
}

func attachWriters(files []disk.File, writers map[string][]disk.Writer) {
	for index := range files {
		files[index].Writers = writers[files[index].Path]
	}
}

func Discover() Report {
	report := Report{Runtime: "docker"}
	paths := make([]PathReport, 0)
	configPaths := []string{"/etc/docker/daemon.json"}
	if current, err := user.Current(); err == nil {
		rootlessConfig := filepath.Join(current.HomeDir, ".config", "docker", "daemon.json")
		configPaths = append(configPaths, rootlessConfig)
	}
	for _, configPath := range configPaths {
		config, err := readConfig(configPath)
		if err != nil {
			continue
		}
		report.Config = configPath
		if config.DataRoot != "" {
			report.DataRoot = config.DataRoot
		}
		break
	}
	if report.DataRoot == "" {
		report.DataRoot = firstExisting([]string{"/var/lib/docker", rootlessDataRoot()})
	}
	if report.DataRoot != "" {
		paths = append(paths, PathReport{Path: report.DataRoot, Kind: "docker data root"})
	}
	for _, candidate := range []struct {
		path string
		kind string
	}{
		{"/var/lib/containerd", "containerd storage"},
		{"/var/log/docker", "Docker logs"},
		{"/var/lib/buildkit", "BuildKit cache"},
	} {
		if exists(candidate.path) && candidate.path != report.DataRoot {
			paths = append(paths, PathReport{Path: candidate.path, Kind: candidate.kind})
		}
	}
	report.Paths = uniquePaths(paths)
	report.Socket = firstExisting([]string{"/var/run/docker.sock", rootlessSocket()})
	report.Rootless = strings.Contains(report.DataRoot, "/.local/share/docker") || strings.Contains(report.Socket, "/run/user/")
	report.Processes = processes()
	return report
}

func printReport(writer io.Writer, report Report) error {
	checked := textfmt.NewWriter(writer)
	writer = checked
	fmt.Fprintln(writer, "Docker storage")
	fmt.Fprintf(writer, "Runtime: %s", textfmt.Safe(report.Runtime))
	if report.Rootless {
		fmt.Fprint(writer, " (rootless)")
	}
	fmt.Fprintln(writer)
	if report.Config != "" {
		fmt.Fprintf(writer, "Config: %s\n", textfmt.Safe(report.Config))
	}
	if report.DataRoot != "" {
		fmt.Fprintf(writer, "Data root: %s\n", textfmt.Safe(report.DataRoot))
	}
	if report.Socket != "" {
		fmt.Fprintf(writer, "Socket: %s\n", textfmt.Safe(report.Socket))
	}
	if len(report.Processes) > 0 {
		fmt.Fprintln(writer, "Processes")
		for _, process := range report.Processes {
			fmt.Fprintf(writer, "  PID %d  %s  %s\n", process.PID, textfmt.Safe(process.Process), textfmt.Safe(process.Exe))
		}
	}
	for _, path := range report.Paths {
		fmt.Fprintf(writer, "\n%s: %s\n", textfmt.Safe(path.Kind), textfmt.Safe(path.Path))
		if path.Error != "" {
			fmt.Fprintf(writer, "  Unavailable: %s\n", textfmt.Safe(path.Error))
			for _, warning := range path.Warnings {
				fmt.Fprintf(writer, "  Warning: %s\n", textfmt.Safe(warning))
			}
			continue
		}
		fmt.Fprintf(writer, "  Pressure: %.1f%%  Used: %.1f%%  Inodes: %.1f%%  Available: %s\n", path.Usage.PressurePercent, path.Usage.UsedPercent, path.Usage.InodePercent, formatBytes(path.Usage.AvailableBytes))
		if path.Usage.GrowthBytesPerSecond > 0 {
			fmt.Fprintf(writer, "  Filesystem net trend: +%s/hour\n", formatBytes(uint64(path.Usage.GrowthBytesPerSecond*3600)))
		}
		if len(path.Files) > 0 {
			fmt.Fprintln(writer, "  Largest files")
			for _, file := range path.Files {
				fmt.Fprintf(writer, "    %10s  %s%s\n", formatBytes(uint64(file.AllocatedBytes)), textfmt.Safe(file.Path), growth(file.Growth))
			}
		}
		if len(path.Growing) > 0 {
			fmt.Fprintln(writer, "  Fastest growth")
			for _, file := range path.Growing {
				fmt.Fprintf(writer, "    %10s/s  %s\n", formatBytes(uint64(file.Growth)), textfmt.Safe(file.Path))
			}
		}
		if len(path.Directories) > 0 {
			fmt.Fprintln(writer, "  Largest directories")
			for _, directory := range path.Directories[:min(5, len(path.Directories))] {
				fmt.Fprintf(writer, "    %10s  %s\n", formatBytes(uint64(directory.AllocatedBytes)), textfmt.Safe(directory.Path))
			}
		}
		for _, warning := range path.Warnings {
			fmt.Fprintf(writer, "  Warning: %s\n", textfmt.Safe(warning))
		}
	}
	if len(report.Paths) == 0 {
		fmt.Fprintln(writer, "No Docker storage paths found.")
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(writer, "Warning: %s\n", textfmt.Safe(warning))
	}
	return checked.Err()
}

func growth(value float64) string {
	if value > 0 {
		return fmt.Sprintf("  ↑ %s/s", formatBytes(uint64(value)))
	}
	if value < 0 {
		return fmt.Sprintf("  ↓ %s/s", formatBytes(uint64(-value)))
	}
	return ""
}

func readConfig(path string) (daemonConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return daemonConfig{}, err
	}
	var config daemonConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return daemonConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return config, nil
}

func processes() []Process {
	result := make([]Process, 0)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if _, err := os.Stat(filepath.Join("/proc", entry.Name(), "comm")); err != nil {
			continue
		}
		name := strings.TrimSpace(string(readFile(filepath.Join("/proc", entry.Name(), "comm"))))
		if name != "dockerd" && name != "dockerd-rootless.sh" && name != "containerd" && !strings.HasPrefix(name, "containerd-shim") {
			continue
		}
		pid := 0
		if _, err := fmt.Sscanf(entry.Name(), "%d", &pid); err != nil {
			continue
		}
		exe, _ := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
		result = append(result, Process{PID: pid, Process: name, Exe: exe})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PID < result[j].PID })
	return result
}

func rootlessDataRoot() string {
	current, err := user.Current()
	if err != nil {
		return ""
	}
	return filepath.Join(current.HomeDir, ".local", "share", "docker")
}

func rootlessSocket() string {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "docker.sock")
	}
	return ""
}

func firstExisting(paths []string) string {
	for _, path := range paths {
		if exists(path) {
			return path
		}
	}
	return ""
}

func exists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func uniquePaths(paths []PathReport) []PathReport {
	seen := make(map[string]bool)
	result := make([]PathReport, 0, len(paths))
	for _, path := range paths {
		if !seen[path.Path] {
			seen[path.Path] = true
			result = append(result, path)
		}
	}
	return result
}

func readFile(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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
