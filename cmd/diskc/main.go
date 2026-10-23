package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/debugc-clis/diskc/internal/buildinfo"
	"github.com/debugc-clis/diskc/internal/cliutil"
	"github.com/debugc-clis/diskc/internal/database"
	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/docker"
	"github.com/debugc-clis/diskc/internal/doctor"
	"github.com/debugc-clis/diskc/internal/health"
	"github.com/debugc-clis/diskc/internal/inventory"
	"github.com/debugc-clis/diskc/internal/macosystem"
	"github.com/debugc-clis/diskc/internal/mount"
	"github.com/debugc-clis/diskc/internal/proc"
	"github.com/debugc-clis/diskc/internal/report"
	"github.com/debugc-clis/diskc/internal/settings"
	"github.com/debugc-clis/diskc/internal/textfmt"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "list":
			return finish(inventory.Run(args[1:], stdout, stderr), stderr)
		case "db":
			return finish(database.Run(args[1:], stdout, stderr), stderr)
		case "docker":
			return finish(docker.Run(args[1:], stdout, stderr), stderr)
		case "doc":
			return finish(doctor.Run(args[1:], stdout), stderr)
		case "mac":
			if len(args) < 2 || args[1] != "system" {
				return finish(cliutil.Usage(fmt.Errorf("mac: expected subcommand 'system'")), stderr)
			}
			return finish(macosystem.Run(args[2:], stdout, stderr), stderr)
		case "version", "--version", "-version":
			if len(args) != 1 {
				return finish(cliutil.Usage(fmt.Errorf("version: unexpected argument %q", args[1])), stderr)
			}
			_, err := fmt.Fprintln(stdout, buildinfo.String())
			return finish(err, stderr)
		}
	}

	mode, commandArgs := parseMode(args)
	config := defaultScanConfig(mode)
	watch := false
	interval := settings.WatchInterval
	flags := flag.NewFlagSet("diskc", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.IntVar(&config.top, "top", config.top, "number of large files to report")
	flags.IntVar(&config.depth, "depth", config.depth, "maximum directory depth to scan")
	flags.DurationVar(&config.sample, "sample", config.sample, "measure growth over this duration; use 0 to disable, for example 5s")
	flags.BoolVar(&config.jsonOutput, "json", false, "emit machine-readable JSON")
	flags.BoolVar(&config.all, "all", false, "inspect all mounted local filesystems")
	flags.BoolVar(&watch, "watch", false, "refresh the report continuously until interrupted")
	flags.DurationVar(&interval, "interval", interval, "delay between completed watch scans, for example 3s")
	flags.Usage = func() { printUsage(stdout, flags) }
	if err := flags.Parse(normalizeRootArgs(removeLegacyArgs(commandArgs))); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return finish(cliutil.Usage(err), stderr)
	}
	if config.top < 1 || config.top > disk.MaxReportFiles || config.depth < 0 || config.sample < 0 || interval <= 0 {
		return finish(cliutil.Usage(fmt.Errorf("top must be between 1 and %d; depth and sample must be non-negative; interval must be positive", disk.MaxReportFiles)), stderr)
	}
	roots := flags.Args()
	if len(roots) == 0 {
		roots = []string{"/"}
	}
	if watch {
		err := watchLoop(context.Background(), stdout, stderr, roots, config, interval, writerIsTerminal(stdout), refresh)
		return finish(err, stderr)
	}
	return finish(refresh(stdout, stderr, roots, config), stderr)
}

func printUsage(writer io.Writer, flags *flag.FlagSet) {
	fmt.Fprintln(writer, "Usage: diskc [path ...] [flags]")
	fmt.Fprintln(writer, "       diskc light [path ...] [flags]")
	fmt.Fprintln(writer, "       diskc list [--json]")
	fmt.Fprintln(writer, "       diskc db [flags]")
	fmt.Fprintln(writer, "       diskc docker [flags]")
	fmt.Fprintln(writer, "       diskc doc [path] [--json]")
	fmt.Fprintln(writer, "       diskc mac system [flags]")
	fmt.Fprintln(writer, "       diskc version")
	fmt.Fprintln(writer, "\nFind what is filling a filesystem. Use 'diskc list' for an instant mounted-filesystem summary.\n\nFlags:")
	flags.SetOutput(writer)
	flags.PrintDefaults()
}

func finish(err error, stderr io.Writer) int {
	if err == nil || isBrokenPipe(err) {
		return 0
	}
	fmt.Fprintf(stderr, "diskc: %s\n", textfmt.Safe(err.Error()))
	if cliutil.IsUsage(err) {
		return 2
	}
	return 1
}

func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE)
}

type runMode string

const (
	standardMode runMode = "standard"
	lightMode    runMode = "light"
)

type scanConfig struct {
	mode             runMode
	top              int
	depth            int
	sample           time.Duration
	all              bool
	jsonOutput       bool
	jsonStream       bool
	inspectProcesses bool
}

func parseMode(args []string) (runMode, []string) {
	if len(args) > 0 && args[0] == string(lightMode) {
		return lightMode, args[1:]
	}
	return standardMode, args
}

func defaultScanConfig(mode runMode) scanConfig {
	if mode == lightMode {
		return scanConfig{mode: mode, top: settings.LightTop, depth: settings.LightDepth}
	}
	return scanConfig{mode: mode, top: settings.Top, depth: settings.Depth, sample: settings.Sample, inspectProcesses: true}
}

type scannedTarget struct {
	root        string
	filesystem  disk.Filesystem
	files       []disk.File
	directories []disk.Directory
	warnings    []string
	health      health.Snapshot
}

type refreshFunc func(io.Writer, io.Writer, []string, scanConfig) error

func watchLoop(ctx context.Context, stdout, stderr io.Writer, roots []string, config scanConfig, interval time.Duration, terminal bool, scan refreshFunc) error {
	config.jsonStream = config.jsonOutput
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if !config.jsonOutput {
			if terminal {
				if _, err := fmt.Fprint(stdout, "\033[H\033[2J"); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(stdout, "--- diskc refresh %s ---\n", time.Now().UTC().Format(time.RFC3339)); err != nil {
					return err
				}
			}
		}
		if err := scan(stdout, stderr, roots, config); err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func writerIsTerminal(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func refresh(writer, stderr io.Writer, roots []string, config scanConfig) error {
	if config.all {
		mounts, err := mount.All()
		if err != nil {
			return err
		}
		roots = append(append([]string{}, roots...), mounts...)
	}
	targets := make([]scannedTarget, 0, len(roots))
	seenDevices := make(map[uint64]struct{})
	for _, root := range unique(roots) {
		filesystem, err := disk.FilesystemUsage(root)
		if err != nil {
			fmt.Fprintf(stderr, "diskc: skipping %s: %s\n", textfmt.Safe(root), textfmt.Safe(err.Error()))
			continue
		}
		if config.all {
			if _, seen := seenDevices[filesystem.Device]; seen {
				continue
			}
		}
		files, directories, warnings, err := disk.Scan(root, filesystem.Device, config.depth, config.top)
		if err != nil {
			fmt.Fprintf(stderr, "diskc: skipping %s: %s\n", textfmt.Safe(root), textfmt.Safe(err.Error()))
			continue
		}
		if config.all {
			seenDevices[filesystem.Device] = struct{}{}
		}
		targets = append(targets, scannedTarget{root: root, filesystem: filesystem, files: files, directories: directories, warnings: warnings})
	}
	if len(targets) == 0 {
		return fmt.Errorf("no filesystems could be inspected")
	}
	groups := make([][]disk.File, len(targets))
	for index := range targets {
		groups[index] = targets[index].files
		if config.sample > 0 {
			targets[index].health = health.TakeSnapshot(targets[index].filesystem.Device)
		}
	}
	if err := disk.MeasureGrowthGroups(groups, config.sample); err != nil {
		for index := range targets {
			targets[index].warnings = append(targets[index].warnings, err.Error())
		}
	}

	var deletedFiles []proc.DeletedFile
	if config.inspectProcesses {
		deletedFiles = proc.DeletedFiles()
	}
	outputs := make([]report.Data, 0, len(targets))
	candidates := make([]disk.File, 0)
	seenDeletedDevices := make(map[uint64]struct{})
	hostFindings := health.HostFindings()
	for index := range targets {
		target := &targets[index]
		disk.UpdateTrend(target.files, &target.filesystem)
		largest, growing := disk.SelectReportFiles(target.files, config.top)
		if config.inspectProcesses {
			candidates = append(candidates, largest...)
			candidates = append(candidates, growing...)
		}
		findings := health.FilesystemFindings(target.root, target.filesystem.Device, target.health, config.sample.Seconds())
		if index == 0 {
			findings = append(findings, hostFindings...)
		}
		var deleted []proc.DeletedFile
		if _, seen := seenDeletedDevices[target.filesystem.Device]; !seen {
			deleted = deletedForDevice(deletedFiles, target.filesystem.Device)
			seenDeletedDevices[target.filesystem.Device] = struct{}{}
		}
		outputs = append(outputs, report.Data{
			Mode: reportMode(config.mode), Filesystem: target.filesystem, Files: largest,
			Growing: growing, Directories: target.directories, Deleted: deleted,
			Findings: findings, Warnings: target.warnings,
		})
	}
	if config.inspectProcesses {
		proc.AttributeWriters(candidates)
	}
	writers := make(map[string][]disk.Writer, len(candidates))
	for _, file := range candidates {
		if len(file.Writers) > 0 {
			writers[file.Path] = file.Writers
		}
	}
	for index := range outputs {
		attachWriters(outputs[index].Files, writers)
		attachWriters(outputs[index].Growing, writers)
	}
	if config.jsonStream {
		return report.PrintManyJSONStream(writer, outputs)
	}
	return report.PrintMany(writer, outputs, config.jsonOutput)
}

func reportMode(mode runMode) string {
	if mode == lightMode {
		return string(mode)
	}
	return ""
}

func attachWriters(files []disk.File, writers map[string][]disk.Writer) {
	for index := range files {
		files[index].Writers = writers[files[index].Path]
	}
}

func deletedForDevice(files []proc.DeletedFile, device uint64) []proc.DeletedFile {
	result := make([]proc.DeletedFile, 0)
	for _, file := range files {
		if file.Device == device {
			result = append(result, file)
		}
	}
	return result
}

func removeLegacyArgs(args []string) []string {
	result := make([]string, 0, len(args))
	for _, arg := range args {
		if arg != "--deleted" && arg != "-deleted" {
			result = append(result, arg)
		}
	}
	return result
}

func normalizeRootArgs(args []string) []string {
	valueFlags := map[string]bool{"top": true, "depth": true, "sample": true, "interval": true}
	flags := make([]string, 0, len(args))
	paths := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			paths = append(paths, args[index+1:]...)
			break
		}
		name, hasValue := flagName(arg)
		if name == "" {
			paths = append(paths, arg)
			continue
		}
		flags = append(flags, arg)
		if valueFlags[name] && !hasValue && index+1 < len(args) {
			index++
			flags = append(flags, args[index])
		}
	}
	return append(flags, paths...)
}

func flagName(arg string) (string, bool) {
	if len(arg) < 2 || arg[0] != '-' || arg == "-" {
		return "", false
	}
	name := arg[1:]
	if len(name) > 0 && name[0] == '-' {
		name = name[1:]
	}
	for index, character := range name {
		if character == '=' {
			return name[:index], true
		}
	}
	return name, false
}

func unique(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	return result
}
