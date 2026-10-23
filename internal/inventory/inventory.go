package inventory

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/debugc-clis/diskc/internal/cliutil"
	"github.com/debugc-clis/diskc/internal/disk"
	"github.com/debugc-clis/diskc/internal/mount"
	"github.com/debugc-clis/diskc/internal/textfmt"
)

type Entry struct {
	Status          string  `json:"status"`
	Mount           string  `json:"mount"`
	Source          string  `json:"source"`
	Type            string  `json:"type"`
	Kind            string  `json:"kind"`
	Mode            string  `json:"mode"`
	TotalBytes      uint64  `json:"total_bytes"`
	UsedBytes       uint64  `json:"used_bytes"`
	FreeBytes       uint64  `json:"free_bytes"`
	AvailableBytes  uint64  `json:"available_bytes"`
	ReservedBytes   uint64  `json:"reserved_bytes"`
	UsedPercent     float64 `json:"used_percent"`
	PressurePercent float64 `json:"pressure_percent"`
	InodePercent    float64 `json:"inode_percent"`
	SharedCapacity  bool    `json:"shared_capacity,omitempty"`
	Error           string  `json:"error,omitempty"`
}

var listMounts = mount.List
var filesystemUsage = disk.FilesystemUsage

func Run(args []string, stdout, _ io.Writer) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	flags.Usage = func() {
		fmt.Fprintln(stdout, "Usage: diskc list [--json]\n\nList mounted filesystems without scanning their contents.")
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
		return cliutil.Usage(fmt.Errorf("list: unexpected argument %q", flags.Arg(0)))
	}
	entries, err := Collect()
	if err != nil {
		return err
	}
	return Print(stdout, entries, *jsonOutput)
}

func Collect() ([]Entry, error) {
	mounts, err := listMounts()
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(mounts))
	for _, item := range mounts {
		entry := Entry{
			Mount:  item.Path,
			Source: item.Source,
			Type:   item.Type,
			Kind:   item.Kind,
			Mode:   "rw",
		}
		if item.ReadOnly {
			entry.Mode = "ro"
		}
		if item.Kind == mount.KindRemote {
			entry.Status = "REMOTE"
			entries = append(entries, entry)
			continue
		}
		if item.Kind == mount.KindUnknown {
			entry.Status = "UNKNOWN"
			entries = append(entries, entry)
			continue
		}
		usage, usageErr := filesystemUsage(item.Path)
		if usageErr != nil {
			entry.Status = "UNKNOWN"
			entry.Error = usageErr.Error()
			entries = append(entries, entry)
			continue
		}
		entry.TotalBytes = usage.TotalBytes
		entry.UsedBytes = usage.UsedBytes
		entry.FreeBytes = usage.FreeBytes
		entry.AvailableBytes = usage.AvailableBytes
		entry.ReservedBytes = usage.ReservedBytes
		entry.UsedPercent = usage.UsedPercent
		entry.PressurePercent = usage.PressurePercent
		entry.InodePercent = usage.InodePercent
		entry.Status = pressureStatus(entry.PressurePercent, entry.InodePercent)
		entries = append(entries, entry)
	}
	markSharedCapacity(entries)
	return entries, nil
}

func Print(writer io.Writer, entries []Entry, jsonOutput bool) error {
	ordered := append([]Entry(nil), entries...)
	sortEntries(ordered)
	if jsonOutput {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(ordered)
	}
	if len(ordered) == 0 {
		_, err := fmt.Fprintln(writer, "No mounted filesystems found.")
		return err
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "STATUS\tMOUNT\tSOURCE\tTYPE/KIND\tSIZE\tUSED\tAVAIL\tPRESS%\tINODE%\tMODE\tCAPACITY"); err != nil {
		return err
	}
	for _, entry := range ordered {
		size, used, available, pressurePercent, inodePercent := "-", "-", "-", "-", "-"
		if entry.Error == "" && entry.TotalBytes > 0 {
			size = formatBytes(entry.TotalBytes)
			used = formatBytes(entry.UsedBytes)
			available = formatBytes(entry.AvailableBytes)
			pressurePercent = fmt.Sprintf("%.1f%%", entry.PressurePercent)
			inodePercent = fmt.Sprintf("%.1f%%", entry.InodePercent)
		}
		capacity := "dedicated"
		if entry.SharedCapacity {
			capacity = "shared"
		} else if entry.TotalBytes == 0 {
			capacity = "-"
		}
		filesystemType := display(entry.Type) + "/" + display(entry.Kind)
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			textfmt.Safe(entry.Status), textfmt.Safe(entry.Mount), textfmt.Safe(display(entry.Source)), textfmt.Safe(filesystemType), size, used, available, pressurePercent, inodePercent, textfmt.Safe(entry.Mode), capacity); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	for _, entry := range ordered {
		if entry.Error != "" {
			if _, err := fmt.Fprintf(writer, "Warning: cannot inspect %s: %s\n", textfmt.Safe(entry.Mount), textfmt.Safe(entry.Error)); err != nil {
				return err
			}
		}
	}
	return nil
}

func pressureStatus(usedPercent, inodePercent float64) string {
	if usedPercent >= 95 || inodePercent >= 95 {
		return "CRITICAL"
	}
	if usedPercent >= 90 || inodePercent >= 90 {
		return "WARNING"
	}
	return "OK"
}

func markSharedCapacity(entries []Entry) {
	for left := range entries {
		if entries[left].Type != "apfs" || entries[left].TotalBytes == 0 {
			continue
		}
		for right := left + 1; right < len(entries); right++ {
			if entries[right].Type == entries[left].Type &&
				entries[right].TotalBytes == entries[left].TotalBytes &&
				entries[right].FreeBytes == entries[left].FreeBytes &&
				entries[right].AvailableBytes == entries[left].AvailableBytes {
				entries[left].SharedCapacity = true
				entries[right].SharedCapacity = true
			}
		}
	}
}

func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if statusRank(entries[i].Status) != statusRank(entries[j].Status) {
			return statusRank(entries[i].Status) < statusRank(entries[j].Status)
		}
		if entries[i].UsedPercent != entries[j].UsedPercent {
			return entries[i].UsedPercent > entries[j].UsedPercent
		}
		return entries[i].Mount < entries[j].Mount
	})
}

func statusRank(status string) int {
	switch status {
	case "CRITICAL":
		return 0
	case "WARNING":
		return 1
	case "UNKNOWN":
		return 2
	case "REMOTE":
		return 3
	default:
		return 4
	}
}

func display(value string) string {
	if value == "" {
		return "-"
	}
	return value
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
