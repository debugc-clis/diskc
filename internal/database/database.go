package database

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/proc"
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
	Directories []disk.Directory `json:"directories,omitempty"`
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

func Run(args []string) error {
	flags := flag.NewFlagSet("db", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	top := flags.Int("top", 20, "number of large database files to report")
	depth := flags.Int("depth", 4, "maximum database directory depth to scan")
	sample := flags.Duration("sample", 3*time.Second, "measure database file growth over this duration")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *top < 1 || *depth < 0 || *sample < 0 {
		return fmt.Errorf("db: top must be positive; depth and sample must be non-negative")
	}
	instances := Discover()
	if len(instances) == 0 {
		if *jsonOutput {
			_, err := fmt.Fprintln(os.Stdout, "[]")
			return err
		}
		fmt.Fprintln(os.Stdout, "No supported database configuration, data directory, or process found.")
		return nil
	}
	for index := range instances {
		for _, path := range instances[index].DataPaths {
			usage, err := disk.FilesystemUsage(path)
			if err != nil {
				continue
			}
			files, directories, _, err := disk.Scan(path, usage.Device, *depth, *top)
			if err != nil {
				continue
			}
			if *sample > 0 {
				_ = disk.MeasureGrowth(files, *sample)
			}
			disk.UpdateTrend(files, &usage)
			proc.AttributeWriters(files)
			instances[index].Filesystems = append(instances[index].Filesystems, Filesystem{Path: path, Usage: usage, Files: files, Directories: directories})
		}
	}
	if len(instances) == 0 {
		return fmt.Errorf("db: no supported database configuration, data directory, or process found")
	}
	return Print(instances, *jsonOutput)
}

func Print(data []Instance, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(data)
	}
	for _, instance := range data {
		fmt.Printf("Database: %s\n", instance.Engine)
		if len(instance.Processes) > 0 {
			fmt.Println("Processes")
			for _, process := range instance.Processes {
				fmt.Printf("  PID %d  %s  %s\n", process.PID, process.Process, process.Exe)
			}
		}
		if len(instance.Configs) > 0 {
			fmt.Println("Configuration")
			for _, config := range instance.Configs {
				fmt.Printf("  %s\n", config)
			}
		}
		if len(instance.Settings) > 0 {
			fmt.Println("Relevant settings")
			keys := make([]string, 0, len(instance.Settings))
			for key := range instance.Settings {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				fmt.Printf("  %-28s %s\n", key, instance.Settings[key])
			}
		}
		if len(instance.DataPaths) > 0 {
			fmt.Println("Data paths")
			for _, path := range instance.DataPaths {
				fmt.Printf("  %s\n", path)
			}
		}
		if len(instance.LogPaths) > 0 {
			fmt.Println("Log paths")
			for _, path := range instance.LogPaths {
				fmt.Printf("  %s\n", path)
			}
		}
		for _, filesystem := range instance.Filesystems {
			fmt.Printf("\nFilesystem: %s\n", filesystem.Path)
			fmt.Printf("  Used: %.1f%%  Inodes: %.1f%%  Free: %s\n", filesystem.Usage.UsedPercent, filesystem.Usage.InodePercent, formatBytes(filesystem.Usage.FreeBytes))
			if filesystem.Usage.GrowthBytesPerSecond > 0 {
				fmt.Printf("  Trend: +%s/hour\n", formatBytes(uint64(filesystem.Usage.GrowthBytesPerSecond*3600)))
			}
			for _, file := range filesystem.Files {
				growth := ""
				if file.Growth != 0 {
					growth = fmt.Sprintf("  ↑ %s/s", formatBytes(uint64(abs(file.Growth))))
				}
				fmt.Printf("  %10s  %s%s\n", formatBytes(uint64(file.Size)), file.Path, growth)
			}
		}
	}
	return nil
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
