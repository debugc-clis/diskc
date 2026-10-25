package database

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/debugc-clis/diskc/internal/cliutil"
	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/proc"
	"github.com/debugc-clis/diskc/internal/settings"
	"github.com/debugc-clis/diskc/internal/textfmt"
)

type Instance struct {
	Engine      string            `json:"engine"`
	Configs     []string          `json:"configs,omitempty"`
	Settings    map[string]string `json:"settings,omitempty"`
	DataPaths   []string          `json:"data_paths,omitempty"`
	LogPaths    []string          `json:"log_paths,omitempty"`
	Processes   []Process         `json:"processes,omitempty"`
	Filesystems []Filesystem      `json:"filesystems,omitempty"`
}

type Process struct {
	PID     int    `json:"pid"`
	Process string `json:"process"`
	Exe     string `json:"exe,omitempty"`
}

type Filesystem struct {
	Path        string           `json:"path"`
	Usage       disk.Filesystem  `json:"usage"`
	Files       []disk.File      `json:"files,omitempty"`
	Growing     []disk.File      `json:"fastest_growing,omitempty"`
	Directories []disk.Directory `json:"directories,omitempty"`
	Warnings    []string         `json:"warnings,omitempty"`
	Error       string           `json:"error,omitempty"`
	candidates  []disk.File
}

type definition struct {
	Engine      string
	ConfigGlobs []string
	DataPaths   []string
}

var definitions = []definition{
	{Engine: "postgresql", ConfigGlobs: []string{"/etc/postgresql/*/*/postgresql.conf", "/var/lib/pgsql/data/postgresql.conf"}, DataPaths: []string{"/var/lib/postgresql", "/var/lib/pgsql"}},
	{Engine: "mysql", ConfigGlobs: []string{"/etc/my.cnf", "/etc/mysql/my.cnf", "/etc/mysql/mysql.conf.d/*.cnf", "/etc/mysql/mariadb.conf.d/*.cnf"}, DataPaths: []string{"/var/lib/mysql"}},
	{Engine: "redis", ConfigGlobs: []string{"/etc/redis/redis.conf", "/etc/redis/*.conf"}, DataPaths: []string{"/var/lib/redis"}},
	{Engine: "clickhouse", ConfigGlobs: []string{"/etc/clickhouse-server/config.xml"}, DataPaths: []string{"/var/lib/clickhouse"}},
	{Engine: "mongodb", ConfigGlobs: []string{"/etc/mongod.conf", "/etc/mongodb.conf"}, DataPaths: []string{"/var/lib/mongodb"}},
}

var discover = Discover
var filesystemUsage = disk.FilesystemUsage
var scan = disk.Scan
var measureGrowthGroups = disk.MeasureGrowthGroupsWithFilesystems
var attributeWriters = proc.AttributeWriters

func Run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("db", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	top := flags.Int("top", settings.Top, "number of large database files to report")
	depth := flags.Int("depth", settings.Depth, "maximum database directory depth to scan")
	sample := flags.Duration("sample", settings.Sample, "measure database file growth over this duration")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	flags.Usage = func() {
		fmt.Fprintln(stdout, "Usage: diskc db [--top N] [--depth N] [--sample DURATION] [--json]\n\nInspect local database configuration and disk usage.")
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
		return cliutil.Usage(fmt.Errorf("db: unexpected argument %q", flags.Arg(0)))
	}
	if *top < 1 || *top > disk.MaxReportFiles || *depth < 0 || *sample < 0 {
		return cliutil.Usage(fmt.Errorf("db: top must be between 1 and %d; depth and sample must be non-negative", disk.MaxReportFiles))
	}
	instances := discover()
	if len(instances) == 0 {
		if *jsonOutput {
			_, err := fmt.Fprintln(stdout, "[]")
			return err
		}
		_, err := fmt.Fprintln(stdout, "No supported database configuration, data directory, or process found.")
		return err
	}
	inspect(instances, *top, *depth, *sample)
	return Print(stdout, instances, *jsonOutput)
}

type filesystemRef struct {
	instance int
	index    int
}

func inspect(instances []Instance, top, depth int, sample time.Duration) {
	refs := make([]filesystemRef, 0)
	for instanceIndex := range instances {
		for _, path := range instances[instanceIndex].DataPaths {
			filesystem := Filesystem{Path: path}
			usage, err := filesystemUsage(path)
			if err != nil {
				filesystem.Error = fmt.Sprintf("cannot inspect: %v", err)
				instances[instanceIndex].Filesystems = append(instances[instanceIndex].Filesystems, filesystem)
				continue
			}
			filesystem.Usage = usage
			files, directories, warnings, err := scan(path, usage.Device, depth, top)
			filesystem.Warnings = append(filesystem.Warnings, warnings...)
			if err != nil {
				filesystem.Error = fmt.Sprintf("cannot scan: %v", err)
				instances[instanceIndex].Filesystems = append(instances[instanceIndex].Filesystems, filesystem)
				continue
			}
			filesystem.candidates = files
			filesystem.Directories = directories
			instances[instanceIndex].Filesystems = append(instances[instanceIndex].Filesystems, filesystem)
			refs = append(refs, filesystemRef{instance: instanceIndex, index: len(instances[instanceIndex].Filesystems) - 1})
		}
	}
	groups := make([][]disk.File, 0, len(refs))
	filesystems := make([]*disk.Filesystem, 0, len(refs))
	for _, ref := range refs {
		groups = append(groups, instances[ref.instance].Filesystems[ref.index].candidates)
		filesystems = append(filesystems, &instances[ref.instance].Filesystems[ref.index].Usage)
	}
	if err := measureGrowthGroups(groups, filesystems, sample); err != nil {
		for _, ref := range refs {
			filesystem := &instances[ref.instance].Filesystems[ref.index]
			filesystem.Warnings = append(filesystem.Warnings, err.Error())
		}
	}
	candidates := make([]disk.File, 0)
	for _, ref := range refs {
		filesystem := &instances[ref.instance].Filesystems[ref.index]
		filesystem.Files, filesystem.Growing = disk.SelectReportFiles(filesystem.candidates, top)
		candidates = append(candidates, filesystem.Files...)
		candidates = append(candidates, filesystem.Growing...)
	}
	attributeWriters(candidates)
	writers := make(map[string][]disk.Writer, len(candidates))
	for _, file := range candidates {
		if len(file.Writers) > 0 {
			writers[file.Path] = file.Writers
		}
	}
	for _, ref := range refs {
		filesystem := &instances[ref.instance].Filesystems[ref.index]
		attachWriters(filesystem.Files, writers)
		attachWriters(filesystem.Growing, writers)
		filesystem.candidates = nil
	}
}

func attachWriters(files []disk.File, writers map[string][]disk.Writer) {
	for index := range files {
		files[index].Writers = writers[files[index].Path]
	}
}

func Print(writer io.Writer, data []Instance, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(data)
	}
	checked := textfmt.NewWriter(writer)
	writer = checked
	for _, instance := range data {
		fmt.Fprintf(writer, "Database: %s\n", textfmt.Safe(instance.Engine))
		if len(instance.Processes) > 0 {
			fmt.Fprintln(writer, "Processes")
			for _, process := range instance.Processes {
				fmt.Fprintf(writer, "  PID %d  %s  %s\n", process.PID, textfmt.Safe(process.Process), textfmt.Safe(process.Exe))
			}
		}
		if len(instance.Configs) > 0 {
			fmt.Fprintln(writer, "Configuration")
			for _, config := range instance.Configs {
				fmt.Fprintf(writer, "  %s\n", textfmt.Safe(config))
			}
		}
		if len(instance.Settings) > 0 {
			fmt.Fprintln(writer, "Relevant settings")
			keys := make([]string, 0, len(instance.Settings))
			for key := range instance.Settings {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				fmt.Fprintf(writer, "  %-28s %s\n", textfmt.Safe(key), textfmt.Safe(instance.Settings[key]))
			}
		}
		if len(instance.DataPaths) > 0 {
			fmt.Fprintln(writer, "Data paths")
			for _, path := range instance.DataPaths {
				fmt.Fprintf(writer, "  %s\n", textfmt.Safe(path))
			}
		}
		if len(instance.LogPaths) > 0 {
			fmt.Fprintln(writer, "Log paths")
			for _, path := range instance.LogPaths {
				fmt.Fprintf(writer, "  %s\n", textfmt.Safe(path))
			}
		}
		for _, filesystem := range instance.Filesystems {
			fmt.Fprintf(writer, "\nFilesystem: %s\n", textfmt.Safe(filesystem.Path))
			if filesystem.Error != "" {
				fmt.Fprintf(writer, "  Unavailable: %s\n", textfmt.Safe(filesystem.Error))
				for _, warning := range filesystem.Warnings {
					fmt.Fprintf(writer, "  Warning: %s\n", textfmt.Safe(warning))
				}
				continue
			}
			fmt.Fprintf(writer, "  Pressure: %.1f%%  Used: %.1f%%  Inodes: %.1f%%  Available: %s\n", filesystem.Usage.PressurePercent, filesystem.Usage.UsedPercent, filesystem.Usage.InodePercent, formatBytes(filesystem.Usage.AvailableBytes))
			if filesystem.Usage.GrowthBytesPerSecond > 0 {
				fmt.Fprintf(writer, "  Filesystem net trend: +%s/hour\n", formatBytes(uint64(filesystem.Usage.GrowthBytesPerSecond*3600)))
			}
			for _, file := range filesystem.Files {
				fmt.Fprintf(writer, "  %10s  %s%s\n", formatBytes(uint64(file.AllocatedBytes)), textfmt.Safe(file.Path), growth(file.Growth))
			}
			if len(filesystem.Growing) > 0 {
				fmt.Fprintln(writer, "  Fastest growth")
				for _, file := range filesystem.Growing {
					fmt.Fprintf(writer, "  %10s/s  %s\n", formatBytes(uint64(file.Growth)), textfmt.Safe(file.Path))
				}
			}
			for _, warning := range filesystem.Warnings {
				fmt.Fprintf(writer, "  Warning: %s\n", textfmt.Safe(warning))
			}
		}
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

func Discover() []Instance {
	processes := processTable()
	instances := make([]Instance, 0)
	for _, definition := range definitions {
		configs := matches(definition.ConfigGlobs)
		settings := make(map[string]string)
		dataPaths := existing(definition.DataPaths)
		logs := make([]string, 0)
		for _, config := range configs {
			parsed, paths := parseConfig(config, definition.Engine)
			for key, value := range parsed {
				settings[key] = value
			}
			dataPaths = append(dataPaths, paths.data...)
			logs = append(logs, paths.logs...)
		}
		dataPaths = uniqueExisting(dataPaths)
		logs = uniqueExisting(logs)
		if len(configs) == 0 && len(dataPaths) == 0 && len(processes[definition.Engine]) == 0 {
			continue
		}
		instances = append(instances, Instance{Engine: definition.Engine, Configs: configs, Settings: settings, DataPaths: dataPaths, LogPaths: logs, Processes: processes[definition.Engine]})
	}
	return instances
}

type parsedPaths struct{ data, logs []string }

func parseConfig(path, engine string) (map[string]string, parsedPaths) {
	settings := make(map[string]string)
	paths := parsedPaths{}
	file, err := os.Open(path)
	if err != nil {
		return settings, paths
	}
	defer file.Close()
	base := filepath.Dir(path)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if line == "" || strings.HasPrefix(line, "<!--") {
			continue
		}
		key, value, ok := setting(line, engine)
		if !ok {
			continue
		}
		if strings.Contains(key, "data_directory") || key == "datadir" || key == "dbpath" || key == "path" || key == "dir" {
			paths.data = append(paths.data, resolve(base, value))
		}
		if strings.Contains(key, "log") || strings.Contains(key, "journal") {
			if strings.Contains(value, "/") {
				paths.logs = append(paths.logs, resolve(base, value))
			}
		}
		if isInteresting(engine, key) {
			settings[key] = value
		}
	}
	return settings, paths
}

func setting(line, engine string) (string, string, bool) {
	if engine == "redis" {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			return fields[0], strings.Join(fields[1:], " "), true
		}
		return "", "", false
	}
	if engine == "clickhouse" && strings.HasPrefix(line, "<") {
		start := strings.Index(line, ">")
		end := strings.LastIndex(line, "</")
		if start > 0 && end > start {
			return strings.Trim(line[1:start], " /"), strings.TrimSpace(line[start+1 : end]), true
		}
	}
	separator := "="
	if engine == "mongodb" {
		separator = ":"
	}
	parts := strings.SplitN(line, separator, 2)
	if len(parts) != 2 {
		return "", "", false
	}
	key := strings.TrimSpace(parts[0])
	value := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
	return key, value, key != ""
}

func isInteresting(engine, key string) bool {
	key = strings.ToLower(key)
	for _, value := range []string{"data", "path", "dir", "log", "wal", "binlog", "journal", "archive", "port", "retention", "expire", "replication", "appendonly", "filename"} {
		if strings.Contains(key, value) {
			return true
		}
	}
	return engine == "postgresql" && key == "shared_buffers"
}

func processTable() map[string][]Process {
	result := make(map[string][]Process)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return result
	}
	names := map[string][]string{"postgresql": {"postgres", "postmaster"}, "mysql": {"mysqld", "mariadbd"}, "redis": {"redis-server"}, "clickhouse": {"clickhouse-server"}, "mongodb": {"mongod"}}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		name := strings.TrimSpace(read(filepath.Join("/proc", entry.Name(), "comm")))
		for engine, candidates := range names {
			for _, candidate := range candidates {
				if name == candidate {
					exe, _ := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
					result[engine] = append(result[engine], Process{PID: pid, Process: name, Exe: exe})
				}
			}
		}
	}
	return result
}

func matches(patterns []string) []string {
	result := make([]string, 0)
	for _, pattern := range patterns {
		matched, _ := filepath.Glob(pattern)
		result = append(result, matched...)
	}
	return uniqueExisting(result)
}

func existing(paths []string) []string {
	return uniqueExisting(paths)
}

func uniqueExisting(paths []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, path := range paths {
		if !filepath.IsAbs(path) || seen[path] {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			seen[path] = true
			result = append(result, path)
		}
	}
	return result
}

func resolve(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
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

func read(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
