package docker

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/proc"
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
	Directories []disk.Directory `json:"directories,omitempty"`
}

type daemonConfig struct {
	DataRoot string `json:"data-root"`
}

func Run(args []string) error {
	flags := flag.NewFlagSet("docker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	top := flags.Int("top", 20, "number of large Docker files to report per path")
	depth := flags.Int("depth", 4, "maximum Docker directory depth to scan")
	sample := flags.Duration("sample", 3*time.Second, "measure Docker file growth over this duration")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *top < 1 || *depth < 0 || *sample < 0 {
		return fmt.Errorf("docker: top must be positive; depth and sample must be non-negative")
	}
	report := Discover()
	for index := range report.Paths {
		path := &report.Paths[index]
		usage, err := disk.FilesystemUsage(path.Path)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("cannot inspect %s: %v", path.Path, err))
			continue
		}
		files, directories, warnings, err := disk.Scan(path.Path, usage.Device, *depth, *top)
		if err != nil {
			report.Warnings = append(report.Warnings, warnings...)
			continue
		}
		if *sample > 0 {
			if err := disk.MeasureGrowth(files, *sample); err != nil {
				report.Warnings = append(report.Warnings, err.Error())
			}
		}
		disk.UpdateTrend(files, &usage)
		proc.AttributeWriters(files)
		path.Usage = usage
		path.Files = files
		path.Directories = directories
		report.Warnings = append(report.Warnings, warnings...)
	}
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	printReport(report)
	return nil
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

func printReport(report Report) {
	fmt.Printf("Docker storage\n")
	fmt.Printf("Runtime: %s", report.Runtime)
	if report.Rootless {
		fmt.Printf(" (rootless)")
	}
	fmt.Println()
	if report.Config != "" {
		fmt.Printf("Config: %s\n", report.Config)
	}
	if report.DataRoot != "" {
		fmt.Printf("Data root: %s\n", report.DataRoot)
	}
	if report.Socket != "" {
		fmt.Printf("Socket: %s\n", report.Socket)
	}
	if len(report.Processes) > 0 {
		fmt.Println("Processes")
		for _, process := range report.Processes {
			fmt.Printf("  PID %d  %s  %s\n", process.PID, process.Process, process.Exe)
		}
	}
	for _, path := range report.Paths {
		fmt.Printf("\n%s: %s\n", path.Kind, path.Path)
		fmt.Printf("  Used: %.1f%%  Inodes: %.1f%%  Free: %s\n", path.Usage.UsedPercent, path.Usage.InodePercent, formatBytes(path.Usage.FreeBytes))
		if path.Usage.GrowthBytesPerSecond > 0 {
			fmt.Printf("  Trend: +%s/hour\n", formatBytes(uint64(path.Usage.GrowthBytesPerSecond*3600)))
		}
		if len(path.Files) > 0 {
			fmt.Println("  Largest files")
			for _, file := range path.Files {
				growth := ""
				if file.Growth != 0 {
					growth = fmt.Sprintf("  ↑ %s/s", formatBytes(uint64(abs(file.Growth))))
				}
				fmt.Printf("    %10s  %s%s\n", formatBytes(uint64(file.Size)), file.Path, growth)
			}
		}
		if len(path.Directories) > 0 {
			fmt.Println("  Largest directories")
			for _, directory := range path.Directories[:min(5, len(path.Directories))] {
				fmt.Printf("    %10s  %s\n", formatBytes(uint64(directory.Size)), directory.Path)
			}
		}
	}
	if len(report.Paths) == 0 {
		fmt.Println("No Docker storage paths found.")
	}
	for _, warning := range report.Warnings {
		fmt.Printf("Warning: %s\n", warning)
	}
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

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
