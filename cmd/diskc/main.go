package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/diskc/diskc/internal/database"
	"github.com/diskc/diskc/internal/disk"
	"github.com/diskc/diskc/internal/docker"
	"github.com/diskc/diskc/internal/health"
	"github.com/diskc/diskc/internal/macosystem"
	"github.com/diskc/diskc/internal/mount"
	"github.com/diskc/diskc/internal/proc"
	"github.com/diskc/diskc/internal/report"
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
		deleted  bool
		jsonOut  bool
		all      bool
		watch    bool
		interval = 3 * time.Second
	)
	flag.IntVar(&top, "top", 20, "number of large files to report")
	flag.IntVar(&depth, "depth", 4, "maximum directory depth to scan")
	flag.DurationVar(&sample, "sample", sample, "measure growth over this duration; use 0 to disable, for example 5s")
	flag.BoolVar(&deleted, "deleted", false, "include deleted files still held open by processes")
	flag.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	flag.BoolVar(&all, "all", false, "inspect all mounted physical filesystems")
	flag.BoolVar(&watch, "watch", false, "refresh the report continuously until interrupted")
	flag.DurationVar(&interval, "interval", interval, "watch refresh interval, for example 3s")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: diskc [path ...] [flags]\n       diskc db [flags]\n       diskc docker [flags]\n       diskc mac system [flags]\n\nFind what is filling a filesystem.\n\nFlags:\n")
		flag.PrintDefaults()
	}
	args := reorderArgs(os.Args[1:])
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
			if err := refresh(os.Stdout, roots, all, top, depth, sample, deleted, jsonOut); err != nil {
				fatal(err)
			}
			if remaining := interval - time.Since(started); remaining > 0 {
				time.Sleep(remaining)
			}
		}
	}
	if err := refresh(os.Stdout, roots, all, top, depth, sample, deleted, jsonOut); err != nil {
		fatal(err)
	}
}

func refresh(writer *os.File, roots []string, all bool, top, depth int, sample time.Duration, deleted, jsonOut bool) error {
	if all {
		mounts, err := mount.All()
		if err != nil {
			return err
		}
		roots = append(append([]string{}, roots...), mounts...)
	}
	outputs := make([]report.Data, 0, len(roots))
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
		healthSnapshot := health.TakeSnapshot(filesystem.Device)
		if sample > 0 {
			if err := disk.MeasureGrowth(files, sample); err != nil {
				warnings = append(warnings, err.Error())
			}
		}
		disk.UpdateTrend(files, &filesystem)
		largest, growing := disk.SelectReportFiles(files, top)
		proc.AttributeWriters(largest)
		proc.AttributeWriters(growing)
		var openDeleted []proc.DeletedFile
		if deleted {
			openDeleted = proc.DeletedFiles()
		}
		findings := health.Findings(root, filesystem.Device, healthSnapshot, sample.Seconds())
		outputs = append(outputs, report.Data{Filesystem: filesystem, Files: largest, Growing: growing, Directories: directories, Deleted: openDeleted, Findings: findings, Warnings: warnings})
	}
	if len(outputs) == 0 {
		return fmt.Errorf("no filesystems could be inspected")
	}
	return report.PrintMany(writer, outputs, jsonOut)
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
	os.Exit(2)
}
