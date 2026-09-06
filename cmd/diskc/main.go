package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/debugc-clis/diskc/internal/database"
	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/docker"
	"github.com/debugc-clis/diskc/internal/health"
	"github.com/debugc-clis/diskc/internal/macosystem"
	"github.com/debugc-clis/diskc/internal/mount"
	"github.com/debugc-clis/diskc/internal/proc"
	"github.com/debugc-clis/diskc/internal/report"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "db" {
		if err := database.Run(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "docker" {
		if err := docker.Run(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "mac" && os.Args[2] == "system" {
		if err := macosystem.Run(os.Args[3:]); err != nil {
			fatal(err)
		}
		return
	}
	var (
		top      int
		depth    int
		sample   = 3 * time.Second
		jsonOut  bool
		all      bool
		watch    bool
		interval = 3 * time.Second
	)
	flag.IntVar(&top, "top", 20, "number of large files to report")
	flag.IntVar(&depth, "depth", 4, "maximum directory depth to scan")
	flag.DurationVar(&sample, "sample", sample, "measure growth over this duration; use 0 to disable, for example 5s")
	flag.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	flag.BoolVar(&all, "all", false, "inspect all mounted physical filesystems")
	flag.BoolVar(&watch, "watch", false, "refresh the report continuously until interrupted")
	flag.DurationVar(&interval, "interval", interval, "watch refresh interval, for example 3s")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: diskc [path ...] [flags]\n       diskc db [flags]\n       diskc docker [flags]\n       diskc mac system [flags]\n\nFind what is filling a filesystem.\n\nFlags:\n")
		flag.PrintDefaults()
	}
	args := reorderArgs(removeLegacyArgs(os.Args[1:]))
	if err := flag.CommandLine.Parse(args); err != nil {
		os.Exit(2)
	}

	if top < 1 || depth < 0 || sample < 0 || interval <= 0 {
		fmt.Fprintln(os.Stderr, "diskc: top must be positive; depth and sample must be non-negative; interval must be positive")
		os.Exit(2)
	}
	roots := make([]string, 0, flag.NArg()+1)
	for index := 0; index < flag.NArg(); index++ {
		roots = append(roots, flag.Arg(index))
	}
	if len(roots) == 0 {
		roots = append(roots, "/")
	}
	if watch {
		for {
			started := time.Now()
			if !jsonOut {
				fmt.Fprint(os.Stdout, "\033[H\033[2J")
			}
			if err := refresh(os.Stdout, roots, all, top, depth, sample, jsonOut); err != nil {
				fatal(err)
			}
			if remaining := interval - time.Since(started); remaining > 0 {
				time.Sleep(remaining)
			}
		}
	}
	if err := refresh(os.Stdout, roots, all, top, depth, sample, jsonOut); err != nil {
		fatal(err)
	}
}

type scannedTarget struct {
	root        string
	filesystem  disk.Filesystem
	files       []disk.File
	directories []disk.Directory
	warnings    []string
	health      health.Snapshot
}

func refresh(writer io.Writer, roots []string, all bool, top, depth int, sample time.Duration, jsonOut bool) error {
	if all {
		mounts, err := mount.All()
		if err != nil {
			return err
		}
		roots = append(append([]string{}, roots...), mounts...)
	}
	targets := make([]scannedTarget, 0, len(roots))
	for _, root := range unique(roots) {
		filesystem, err := disk.FilesystemUsage(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "diskc: skipping %s: %v\n", root, err)
			continue
		}
		files, directories, warnings, err := disk.Scan(root, filesystem.Device, depth, top)
		if err != nil {
			fmt.Fprintf(os.Stderr, "diskc: skipping %s: %v\n", root, err)
			continue
		}
		targets = append(targets, scannedTarget{
			root:        root,
			filesystem:  filesystem,
			files:       files,
			directories: directories,
			warnings:    warnings,
		})
	}
	if len(targets) == 0 {
		return fmt.Errorf("no filesystems could be inspected")
	}
	groups := make([][]disk.File, len(targets))
	for index := range targets {
		groups[index] = targets[index].files
		targets[index].health = health.TakeSnapshot(targets[index].filesystem.Device)
	}
	if err := disk.MeasureGrowthGroups(groups, sample); err != nil {
		for index := range targets {
			targets[index].warnings = append(targets[index].warnings, err.Error())
		}
	}

	deletedFiles := proc.DeletedFiles()
	outputs := make([]report.Data, 0, len(targets))
	candidates := make([]disk.File, 0)
	for index := range targets {
		target := &targets[index]
		disk.UpdateTrend(target.files, &target.filesystem)
		largest, growing := disk.SelectReportFiles(target.files, top)
		candidates = append(candidates, largest...)
		candidates = append(candidates, growing...)
		findings := health.Findings(target.root, target.filesystem.Device, target.health, sample.Seconds())
		outputs = append(outputs, report.Data{
			Filesystem:  target.filesystem,
			Files:       largest,
			Growing:     growing,
			Directories: target.directories,
			Deleted:     deletedForDevice(deletedFiles, target.filesystem.Device),
			Findings:    findings,
			Warnings:    target.warnings,
		})
	}
	proc.AttributeWriters(candidates)
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
	return report.PrintMany(writer, outputs, jsonOut)
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

func reorderArgs(args []string) []string {
	pathCount := 0
	for pathCount < len(args) && !strings.HasPrefix(args[pathCount], "-") {
		pathCount++
	}
	if pathCount == 0 {
		return args
	}
	return append(append([]string{}, args[pathCount:]...), args[:pathCount]...)
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

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "diskc: %v\n", err)
	os.Exit(1)
}
