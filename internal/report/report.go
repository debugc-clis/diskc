package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/health"
	"github.com/debugc-clis/diskc/internal/proc"
)

type Data struct {
	Filesystem  disk.Filesystem    `json:"filesystem"`
	Files       []disk.File        `json:"files"`
	Growing     []disk.File        `json:"fastest_growing,omitempty"`
	Directories []disk.Directory   `json:"directories"`
	Deleted     []proc.DeletedFile `json:"deleted,omitempty"`
	Findings    []health.Finding   `json:"diagnostics,omitempty"`
	Warnings    []string           `json:"warnings,omitempty"`
}

func Print(writer io.Writer, data Data, jsonOutput bool) error {
	return PrintMany(writer, []Data{data}, jsonOutput)
}

func PrintMany(writer io.Writer, data []Data, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		if len(data) == 1 {
			return encoder.Encode(data[0])
		}
		return encoder.Encode(data)
	}
	for index, item := range data {
		if len(data) > 1 {
			if index > 0 {
				fmt.Fprintln(writer)
			}
			fmt.Fprintf(writer, "===== %s =====\n", item.Filesystem.Path)
		}
		if err := text(writer, item); err != nil {
			return err
		}
	}
	return nil
}

func text(writer io.Writer, data Data) error {
	pressure := "OK"
	if data.Filesystem.UsedPercent >= 95 || data.Filesystem.InodePercent >= 95 {
		pressure = "CRITICAL"
	} else if data.Filesystem.UsedPercent >= 90 || data.Filesystem.InodePercent >= 90 {
		pressure = "WARNING"
	}
	fmt.Fprintf(writer, "Disk pressure: %s  %.1f%% used (%s free)\n", pressure, data.Filesystem.UsedPercent, formatBytes(data.Filesystem.FreeBytes))
	fmt.Fprintf(writer, "Inodes: %.1f%% used  |  %s\n", data.Filesystem.InodePercent, data.Filesystem.Path)
	if data.Filesystem.GrowthBytesPerSecond > 0 {
		fmt.Fprintf(writer, "Observed growth: +%s/hour\n", formatBytes(uint64(data.Filesystem.GrowthBytesPerSecond*3600)))
		if data.Filesystem.FullInSeconds > 0 {
			fmt.Fprintf(writer, "Estimated full at observed growth: ~%s\n", duration(data.Filesystem.FullInSeconds))
		}
	}
	fmt.Fprintln(writer)
	if len(data.Files) > 0 {
		fmt.Fprintln(writer, "Largest files")
		for _, file := range data.Files {
			writers := ""
			if len(file.Writers) > 0 {
				writers = fmt.Sprintf("  [%d writer(s)]", len(file.Writers))
			}
			fmt.Fprintf(writer, "%10s  %s  (%s)%s%s\n", formatBytes(uint64(file.Size)), file.Path, file.Kind, growth(file.Growth), writers)
		}
	} else {
		fmt.Fprintln(writer, "No regular files found in the scanned scope.")
	}
	if len(data.Directories) > 0 {
		fmt.Fprintln(writer, "\nLargest scanned directories")
		for _, directory := range data.Directories {
			fmt.Fprintf(writer, "%10s  %s\n", formatBytes(uint64(directory.Size)), directory.Path)
		}
	}
	if len(data.Growing) > 0 {
		fmt.Fprintln(writer, "\nFastest growth")
		for _, file := range data.Growing {
			fmt.Fprintf(writer, "%10s/s  %s  (%s)\n", formatBytes(uint64(file.Growth)), file.Path, file.Kind)
		}
	}
	if hasWriters(allFiles(data)) {
		fmt.Fprintln(writer, "\nActive writers")
		for _, file := range allFiles(data) {
			for _, processWriter := range file.Writers {
				fmt.Fprintf(writer, "%s\n└─ PID %d  %s\n", file.Path, processWriter.PID, processWriter.Process)
				if processWriter.Exe != "" {
					fmt.Fprintf(writer, "   ├─ exe      %s\n", processWriter.Exe)
				}
				if processWriter.Service != "" {
					fmt.Fprintf(writer, "   └─ service  %s\n", processWriter.Service)
				}
				if processWriter.Container != "" {
					fmt.Fprintf(writer, "   └─ container %s\n", processWriter.Container)
				}
			}
		}
	}
	if hasGrowth(data.Growing) {
		fmt.Fprintln(writer, "\nPotential issues")
		for _, file := range data.Growing {
			if file.Growth <= 0 {
				continue
			}
			fmt.Fprintf(writer, "%s\n└─ rapid growth: %s/hour\n", file.Path, formatBytes(uint64(file.Growth*3600)))
		}
		if data.Filesystem.FullInSeconds > 0 {
			fmt.Fprintf(writer, "Filesystem could fill in ~%s at the observed aggregate growth.\n", duration(data.Filesystem.FullInSeconds))
		}
	}
	if len(data.Findings) > 0 {
		fmt.Fprintln(writer, "\nSystem diagnostics")
		for _, finding := range data.Findings {
			fmt.Fprintf(writer, "[%s] %s\n", finding.Severity, finding.Message)
		}
	}
	if len(data.Deleted) > 0 {
		fmt.Fprintf(writer, "\nDeleted but still open (%s retained)\n", formatBytes(deletedBytes(data.Deleted)))
		for _, file := range data.Deleted {
			fmt.Fprintf(writer, "%10s  %s", formatBytes(uint64(file.Size)), file.Path)
			for _, processWriter := range file.Writers {
				fmt.Fprintf(writer, "  ← %s (PID %d)", processWriter.Process, processWriter.PID)
			}
			fmt.Fprintln(writer)
		}
	}
	if len(data.Warnings) > 0 {
		fmt.Fprintln(writer, "\nWarnings")
		visible := data.Warnings
		if len(visible) > 5 {
			visible = visible[:5]
		}
		for _, warning := range visible {
			fmt.Fprintf(writer, "- %s\n", warning)
		}
		if hidden := len(data.Warnings) - len(visible); hidden > 0 {
			fmt.Fprintf(writer, "- %d more warnings omitted; use --json for complete details\n", hidden)
		}
	}
	return nil
}

func allFiles(data Data) []disk.File {
	files := make([]disk.File, 0, len(data.Files)+len(data.Growing))
	seen := make(map[string]bool, len(data.Files)+len(data.Growing))
	for _, group := range [][]disk.File{data.Files, data.Growing} {
		for _, file := range group {
			if !seen[file.Path] {
				files = append(files, file)
				seen[file.Path] = true
			}
		}
	}
	return files
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

func growth(value float64) string {
	if value > 0 {
		return fmt.Sprintf("  ↑ %s/s", formatBytes(uint64(value)))
	}
	if value < 0 {
		return fmt.Sprintf("  ↓ %s/s", formatBytes(uint64(abs(value))))
	}
	return ""
}

func deletedBytes(files []proc.DeletedFile) uint64 {
	var total uint64
	for _, file := range files {
		if file.Size > 0 {
			total += uint64(file.Size)
		}
	}
	return total
}

func hasWriters(files []disk.File) bool {
	for _, file := range files {
		if len(file.Writers) > 0 {
			return true
		}
	}
	return false
}

func hasGrowth(files []disk.File) bool {
	for _, file := range files {
		if file.Growth > 0 {
			return true
		}
	}
	return false
}

func duration(seconds float64) string {
	if seconds < 60 {
		if seconds < 1 {
			return "less than 1 second"
		}
		return fmt.Sprintf("%d seconds", int(seconds))
	}
	minutes := int(seconds / 60)
	if minutes < 60 {
		return fmt.Sprintf("%d minutes", minutes)
	}
	if minutes < 24*60 {
		return fmt.Sprintf("%d hours %d minutes", minutes/60, minutes%60)
	}
	return fmt.Sprintf("%d days %d hours", minutes/(24*60), (minutes/60)%24)
}
