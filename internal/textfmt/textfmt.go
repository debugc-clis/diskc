package textfmt

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/debugc-clis/diskc/internal/disk"
)

type Writer struct {
	writer io.Writer
	err    error
}

func NewWriter(writer io.Writer) *Writer { return &Writer{writer: writer} }

func (writer *Writer) Write(data []byte) (int, error) {
	if writer.err != nil {
		return 0, writer.err
	}
	written, err := writer.writer.Write(data)
	if err != nil {
		writer.err = err
	}
	return written, err
}

func (writer *Writer) Err() error { return writer.err }

// GrowthStatusNote explains why a sampled file has no measured growth rate.
func GrowthStatusNote(status string) string {
	switch status {
	case "replaced":
		return "  [growth unmeasured: file replaced; resample]"
	case "unavailable":
		return "  [growth unmeasured: file unavailable or no longer regular; last observed size]"
	default:
		return ""
	}
}

func Safe(value string) string {
	var result strings.Builder
	for _, character := range value {
		switch character {
		case '\n':
			result.WriteString(`\n`)
		case '\r':
			result.WriteString(`\r`)
		case '\t':
			result.WriteString(`\t`)
		default:
			if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
				if character <= 0xff {
					fmt.Fprintf(&result, `\x%02x`, character)
				} else {
					fmt.Fprintf(&result, `\u%04x`, character)
				}
				continue
			}
			result.WriteRune(character)
		}
	}
	return result.String()
}

// FormatScanScope describes observed scan boundaries without claiming full visibility.
func FormatScanScope(scope *disk.ScanScope) string {
	if scope == nil {
		return ""
	}
	var result strings.Builder
	fmt.Fprintf(&result, "Scan scope: %s | max directory depth %d | %d unique regular file(s) observed\n", Safe(scope.Root), scope.MaxDepth, scope.RegularFiles)
	fmt.Fprintf(&result, "Skipped directory boundaries: depth=%d, other filesystem=%d, pseudo filesystem=%d | symlinks=%d | inspection errors=%d\n", scope.SkippedDepthDirectories, scope.SkippedFilesystemDirectories, scope.SkippedPseudoDirectories, scope.SkippedSymlinks, scope.InspectionErrors)
	fmt.Fprintln(&result, "Directory totals cover observed files only; excluded subtrees and inaccessible files are not included.")
	if scope.SkippedDepthDirectories > 0 {
		fmt.Fprintln(&result, "Next: increase --depth or inspect a specific subtree as a scan root.")
	}
	if scope.SkippedSymlinks > 0 || scope.SkippedFilesystemDirectories > 0 {
		fmt.Fprintln(&result, "Next: inspect linked or mounted data paths explicitly as separate scan roots.")
	}
	if scope.InspectionErrors > 0 {
		fmt.Fprintln(&result, "Next: review scan warnings and access permissions; run as the affected service user when practical.")
	}
	fmt.Fprintln(&result)
	return result.String()
}
