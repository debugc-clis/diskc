package disk

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Filesystem struct {
	Path                 string  `json:"path"`
	Device               uint64  `json:"-"`
	TotalBytes           uint64  `json:"total_bytes"`
	UsedBytes            uint64  `json:"used_bytes"`
	FreeBytes            uint64  `json:"free_bytes"`
	UsedPercent          float64 `json:"used_percent"`
	InodePercent         float64 `json:"inode_percent"`
	GrowthBytesPerSecond float64 `json:"growth_bytes_per_second,omitempty"`
	FullInSeconds        float64 `json:"full_in_seconds,omitempty"`
}

type File struct {
	Path       string   `json:"path"`
	Size       int64    `json:"size_bytes"`
	Kind       string   `json:"kind"`
	Growth     float64  `json:"growth_bytes_per_second"`
	Writers    []Writer `json:"writers,omitempty"`
	ModifiedAt int64    `json:"-"`
}

type Directory struct {
	Path string `json:"path"`
	Size int64  `json:"size_bytes"`
}

type Writer struct {
	PID       int    `json:"pid"`
	Process   string `json:"process"`
	Exe       string `json:"exe"`
	UID       string `json:"uid"`
	Service   string `json:"service,omitempty"`
	Container string `json:"container,omitempty"`
}

func FilesystemUsage(path string) (Filesystem, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Filesystem{}, err
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(absolutePath, &stat); err != nil {
		return Filesystem{}, err
	}
	device, err := deviceOf(absolutePath)
	if err != nil {
		return Filesystem{}, err
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bavail * uint64(stat.Bsize)
	inodeTotal := stat.Files
	inodeFree := stat.Ffree
	used := total - free
	usedPercent := percent(used, total)
	inodePercent := percent(inodeTotal-inodeFree, inodeTotal)
	return Filesystem{Path: absolutePath, Device: device, TotalBytes: total, UsedBytes: used, FreeBytes: free, UsedPercent: usedPercent, InodePercent: inodePercent}, nil
}

func Scan(root string, device uint64, maxDepth, limit int) ([]File, []Directory, []string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, nil, err
	}
	files := make([]File, 0)
	candidateLimit := scanCandidateLimit(limit)
	fileCount := 0
	directorySizes := make(map[string]int64)
	warnings := make([]string, 0)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			warnings = append(warnings, fmt.Sprintf("cannot read %s: %v", path, walkErr))
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != root && (isPseudoFilesystem(path) || depth(root, path) > maxDepth) {
				return filepath.SkipDir
			}
			if path != root {
				entryDevice, statErr := deviceOf(path)
				if statErr == nil && entryDevice != device {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			warnings = append(warnings, fmt.Sprintf("cannot stat %s: %v", path, statErr))
			return nil
		}
		fileDevice, deviceErr := deviceOf(path)
		if deviceErr != nil || fileDevice != device {
			return nil
		}
		fileCount++
		file := File{Path: path, Size: info.Size(), Kind: classify(path), ModifiedAt: info.ModTime().UnixNano()}
		files = append(files, file)
		if len(files) >= candidateLimit*2 {
			files = pruneCandidates(files, candidateLimit)
		}
		for parent := filepath.Dir(path); parent != "." && strings.HasPrefix(parent, root); parent = filepath.Dir(parent) {
			directorySizes[parent] += info.Size()
			if parent == root {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, warnings, err
	}
	files = pruneCandidates(files, candidateLimit)
	if fileCount > len(files) {
		warnings = append(warnings, fmt.Sprintf("growth sampling selected %d candidates from %d regular files; largest and recently modified files were prioritized", len(files), fileCount))
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Size > files[j].Size })
	directories := make([]Directory, 0, len(directorySizes))
	for path, size := range directorySizes {
		directories = append(directories, Directory{Path: path, Size: size})
	}
	sort.Slice(directories, func(i, j int) bool { return directories[i].Size > directories[j].Size })
	if len(directories) > limit {
		directories = directories[:limit]
	}
	return files, directories, warnings, nil
}

func scanCandidateLimit(reportLimit int) int {
	limit := reportLimit * 50
	if limit < 1000 {
		limit = 1000
	}
	if limit > 50000 {
		limit = 50000
	}
	if limit < reportLimit {
		return reportLimit
	}
	return limit
}

func pruneCandidates(files []File, limit int) []File {
	if len(files) <= limit {
		return files
	}
	bySize := append([]File(nil), files...)
	sort.Slice(bySize, func(i, j int) bool { return bySize[i].Size > bySize[j].Size })
	byModified := append([]File(nil), files...)
	sort.Slice(byModified, func(i, j int) bool { return byModified[i].ModifiedAt > byModified[j].ModifiedAt })
	result := make([]File, 0, limit)
	seen := make(map[string]bool, limit)
	keep := func(file File) {
		if len(result) < limit && !seen[file.Path] {
			seen[file.Path] = true
			result = append(result, file)
		}
	}
	for index := 0; index < limit/2; index++ {
		keep(bySize[index])
	}
	for _, file := range byModified {
		keep(file)
		if len(result) == limit {
			break
		}
	}
	return result
}

func SelectReportFiles(files []File, limit int) ([]File, []File) {
	largest := append([]File(nil), files...)
	sort.Slice(largest, func(i, j int) bool { return largest[i].Size > largest[j].Size })
	if len(largest) > limit {
		largest = largest[:limit]
	}
	growing := make([]File, 0, len(files))
	for _, file := range files {
		if file.Growth > 0 {
			growing = append(growing, file)
		}
	}
	sort.Slice(growing, func(i, j int) bool { return growing[i].Growth > growing[j].Growth })
	if len(growing) > limit {
		growing = growing[:limit]
	}
	return largest, growing
}

func MeasureGrowth(files []File, interval time.Duration) error {
	return MeasureGrowthGroups([][]File{files}, interval)
}

func MeasureGrowthGroups(groups [][]File, interval time.Duration) error {
	if interval <= 0 {
		return nil
	}
	fileCount := 0
	for _, files := range groups {
		fileCount += len(files)
	}
	if fileCount == 0 {
		return nil
	}
	before := make(map[string]int64, fileCount)
	for _, files := range groups {
		for _, file := range files {
			info, err := os.Stat(file.Path)
			if err == nil {
				before[file.Path] = info.Size()
			}
		}
	}
	time.Sleep(interval)
	seconds := interval.Seconds()
	for _, files := range groups {
		for index := range files {
			initialSize, found := before[files[index].Path]
			if !found {
				continue
			}
			info, err := os.Stat(files[index].Path)
			if err != nil {
				continue
			}
			files[index].Growth = float64(info.Size()-initialSize) / seconds
			files[index].Size = info.Size()
		}
		sort.Slice(files, func(i, j int) bool {
			if files[i].Growth == files[j].Growth {
				return files[i].Size > files[j].Size
			}
			return files[i].Growth > files[j].Growth
		})
	}
	return nil
}

func UpdateTrend(files []File, filesystem *Filesystem) {
	var growth float64
	for _, file := range files {
		if file.Growth > 0 {
			growth += file.Growth
		}
	}
	filesystem.GrowthBytesPerSecond = growth
	if growth > 0 {
		filesystem.FullInSeconds = float64(filesystem.FreeBytes) / growth
	}
}

func deviceOf(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return uint64(info.Sys().(*syscall.Stat_t).Dev), nil
}

func classify(path string) string {
	name := strings.ToLower(filepath.Base(path))
	lower := strings.ToLower(path)
	switch {
	case strings.HasPrefix(name, "core") || strings.Contains(name, ".core"):
		return "core dump"
	case strings.Contains(lower, "/tmp/") || strings.Contains(lower, "/cache/") || strings.Contains(lower, "/caches/"):
		return "temporary/cache"
	case strings.Contains(name, ".log") || strings.Contains(name, "access") || strings.Contains(name, "error"):
		return "log"
	case strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".gz") || strings.HasSuffix(name, ".zip"):
		return "archive"
	default:
		return "file"
	}
}

func isPseudoFilesystem(path string) bool {
	for _, prefix := range []string{"/proc", "/sys", "/dev", "/run", "/snap"} {
		if path == prefix || strings.HasPrefix(path, prefix+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func depth(root, path string) int {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." {
		return 0
	}
	return len(strings.Split(relative, string(os.PathSeparator)))
}

func percent(value, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(value) * 100 / float64(total)
}
