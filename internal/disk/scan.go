package disk

import (
	"container/heap"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	MaxReportFiles  = 1000
	maxScanWarnings = 100
)

type Filesystem struct {
	Path                 string  `json:"path"`
	Device               uint64  `json:"-"`
	TotalBytes           uint64  `json:"total_bytes"`
	UsedBytes            uint64  `json:"used_bytes"`
	FreeBytes            uint64  `json:"free_bytes"`
	AvailableBytes       uint64  `json:"available_bytes"`
	ReservedBytes        uint64  `json:"reserved_bytes"`
	UsedPercent          float64 `json:"used_percent"`
	PressurePercent      float64 `json:"pressure_percent"`
	InodePercent         float64 `json:"inode_percent"`
	GrowthBytesPerSecond float64 `json:"growth_bytes_per_second,omitempty"`
	GrowthBasis          string  `json:"growth_basis,omitempty"`
	FullInSeconds        float64 `json:"full_in_seconds,omitempty"`
}

type File struct {
	Path           string   `json:"path"`
	AllocatedBytes int64    `json:"allocated_bytes"`
	ApparentBytes  int64    `json:"apparent_bytes"`
	Kind           string   `json:"kind"`
	Growth         float64  `json:"growth_bytes_per_second"`
	GrowthStatus   string   `json:"growth_status,omitempty"`
	Writers        []Writer `json:"writers,omitempty"`
	ModifiedAt     int64    `json:"-"`
	identity       inodeKey
	identityKnown  bool
}

type Directory struct {
	Path           string `json:"path"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	ApparentBytes  int64  `json:"apparent_bytes"`
	Estimated      bool   `json:"estimated,omitempty"`
}

type Writer struct {
	PID       int    `json:"pid"`
	Process   string `json:"process"`
	Exe       string `json:"exe"`
	UID       string `json:"uid"`
	Service   string `json:"service,omitempty"`
	Container string `json:"container,omitempty"`
}

type fileMetadata struct {
	device         uint64
	inode          uint64
	links          uint64
	allocatedBytes int64
	apparentBytes  int64
	modifiedAt     int64
}

type inodeKey struct {
	device uint64
	inode  uint64
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
	free := stat.Bfree * uint64(stat.Bsize)
	available := stat.Bavail * uint64(stat.Bsize)
	if free > total {
		free = total
	}
	if available > free {
		available = free
	}
	used := total - free
	reserved := free - available
	inodeTotal := stat.Files
	inodeFree := stat.Ffree
	if inodeFree > inodeTotal {
		inodeFree = inodeTotal
	}
	return Filesystem{
		Path:            absolutePath,
		Device:          device,
		TotalBytes:      total,
		UsedBytes:       used,
		FreeBytes:       free,
		AvailableBytes:  available,
		ReservedBytes:   reserved,
		UsedPercent:     percent(used, total),
		PressurePercent: percent(total-available, total),
		InodePercent:    percent(inodeTotal-inodeFree, inodeTotal),
	}, nil
}

func Scan(root string, device uint64, maxDepth, limit int) ([]File, []Directory, []string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, nil, err
	}
	// Follow aliases only for the requested root. WalkDir still leaves symlinks
	// found inside that root untouched, keeping traversal within its scope.
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, nil, err
	}
	candidateLimit := scanCandidateLimit(limit)
	largest := newFileHeap(candidateLimit, lessAllocated)
	recent := newFileHeap(candidateLimit, lessModified)
	directories := newDirectoryTracker(candidateLimit)
	warnings := newWarningCollector(maxScanWarnings)
	hardLinks := make(map[inodeKey]struct{})
	fileCount := 0
	hardLinkDuplicates := 0

	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			warnings.add(fmt.Sprintf("cannot read %s: %v", path, walkErr))
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
				info, statErr := entry.Info()
				if statErr != nil {
					warnings.add(fmt.Sprintf("cannot stat %s: %v", path, statErr))
					return filepath.SkipDir
				}
				metadata, metadataErr := metadataFromInfo(info)
				if metadataErr != nil {
					warnings.add(fmt.Sprintf("cannot inspect %s: %v", path, metadataErr))
					return filepath.SkipDir
				}
				if metadata.device != device {
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
			warnings.add(fmt.Sprintf("cannot stat %s: %v", path, statErr))
			return nil
		}
		metadata, metadataErr := metadataFromInfo(info)
		if metadataErr != nil || metadata.device != device {
			return nil
		}
		if metadata.links > 1 {
			key := inodeKey{device: metadata.device, inode: metadata.inode}
			if _, seen := hardLinks[key]; seen {
				hardLinkDuplicates++
				return nil
			}
			hardLinks[key] = struct{}{}
		}
		fileCount++
		file := File{
			Path:           path,
			AllocatedBytes: metadata.allocatedBytes,
			ApparentBytes:  metadata.apparentBytes,
			Kind:           classify(path),
			ModifiedAt:     metadata.modifiedAt,
			identity:       inodeKey{device: metadata.device, inode: metadata.inode},
			identityKnown:  true,
		}
		largest.add(file)
		recent.add(file)
		for parent := filepath.Dir(path); parent != "." && pathWithin(root, parent); parent = filepath.Dir(parent) {
			directories.add(parent, metadata.allocatedBytes, metadata.apparentBytes)
			if parent == root {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, warnings.result(), err
	}

	files := mergeCandidates(largest.files(), recent.files(), candidateLimit)
	if fileCount > len(files) {
		warnings.add(fmt.Sprintf("growth sampling selected %d candidates from %d regular files; largest and recently modified files were prioritized", len(files), fileCount))
	}
	if hardLinkDuplicates > 0 {
		warnings.add(fmt.Sprintf("ignored %d duplicate hard-link path(s) so allocated bytes are counted once", hardLinkDuplicates))
	}
	if directories.approximate {
		warnings.add(fmt.Sprintf("directory totals use bounded heavy-hitter estimates after more than %d directories", candidateLimit))
	}
	sort.Slice(files, func(i, j int) bool { return files[i].AllocatedBytes > files[j].AllocatedBytes })
	return files, directories.result(limit), warnings.result(), nil
}

func scanCandidateLimit(reportLimit int) int {
	if reportLimit <= 0 {
		return 0
	}
	if reportLimit >= MaxReportFiles {
		return 50000
	}
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

func mergeCandidates(largest, recent []File, limit int) []File {
	files := make([]File, 0, len(largest)+len(recent))
	seen := make(map[string]struct{}, len(largest)+len(recent))
	for _, group := range [][]File{largest, recent} {
		for _, file := range group {
			if _, found := seen[file.Path]; found {
				continue
			}
			seen[file.Path] = struct{}{}
			files = append(files, file)
		}
	}
	return pruneCandidates(files, limit)
}

func pruneCandidates(files []File, limit int) []File {
	if limit <= 0 {
		return nil
	}
	if len(files) <= limit {
		return files
	}
	bySize := append([]File(nil), files...)
	sort.Slice(bySize, func(i, j int) bool { return bySize[i].AllocatedBytes > bySize[j].AllocatedBytes })
	byModified := append([]File(nil), files...)
	sort.Slice(byModified, func(i, j int) bool { return byModified[i].ModifiedAt > byModified[j].ModifiedAt })
	result := make([]File, 0, limit)
	seen := make(map[string]struct{}, limit)
	keep := func(file File) {
		if len(result) >= limit {
			return
		}
		if _, found := seen[file.Path]; found {
			return
		}
		seen[file.Path] = struct{}{}
		result = append(result, file)
	}
	for index := 0; index < (limit+1)/2 && index < len(bySize); index++ {
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
	sort.Slice(largest, func(i, j int) bool { return largest[i].AllocatedBytes > largest[j].AllocatedBytes })
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
	return MeasureGrowthGroupsWithFilesystems(groups, nil, interval)
}

var sampleFilesystemUsage = FilesystemUsage

func MeasureGrowthGroupsWithFilesystems(groups [][]File, filesystems []*Filesystem, interval time.Duration) error {
	return measureGrowthGroupsWithFilesystems(groups, filesystems, interval, regularFileMetadata)
}

func measureGrowthGroupsWithFilesystems(groups [][]File, filesystems []*Filesystem, interval time.Duration, sampleFile func(string) (fileMetadata, error)) error {
	if interval <= 0 {
		return nil
	}
	fileCount := 0
	for _, files := range groups {
		fileCount += len(files)
	}
	if fileCount == 0 && len(filesystems) == 0 {
		return nil
	}
	type fileSample struct {
		metadata  fileMetadata
		available bool
	}
	// Overlapping scan roots can contain the same pathname. Keep each baseline
	// independently: rotation or access changes can occur between observations.
	before := make([][]fileSample, len(groups))
	for groupIndex, files := range groups {
		before[groupIndex] = make([]fileSample, len(files))
		for index := range files {
			file := &files[index]
			file.Growth = 0
			file.GrowthStatus = "unavailable"
			metadata, err := sampleFile(file.Path)
			if err == nil {
				before[groupIndex][index] = fileSample{metadata: metadata, available: true}
				file.AllocatedBytes = metadata.allocatedBytes
				file.ApparentBytes = metadata.apparentBytes
				file.ModifiedAt = metadata.modifiedAt
				if file.identityKnown && file.identity != (inodeKey{device: metadata.device, inode: metadata.inode}) {
					file.GrowthStatus = "replaced"
				}
			}
		}
	}
	initialUsage := make(map[uint64]uint64, len(filesystems))
	for _, filesystem := range filesystems {
		if _, found := initialUsage[filesystem.Device]; found {
			continue
		}
		usage, err := sampleFilesystemUsage(filesystem.Path)
		if err != nil {
			return err
		}
		initialUsage[filesystem.Device] = usage.AvailableBytes
	}
	started := time.Now()
	time.Sleep(interval)
	seconds := time.Since(started).Seconds()
	finalUsage := make(map[uint64]Filesystem, len(filesystems))
	for _, filesystem := range filesystems {
		if _, found := finalUsage[filesystem.Device]; found {
			continue
		}
		usage, err := sampleFilesystemUsage(filesystem.Path)
		if err != nil {
			return err
		}
		finalUsage[filesystem.Device] = usage
	}
	for _, filesystem := range filesystems {
		usage := finalUsage[filesystem.Device]
		growth := (float64(initialUsage[filesystem.Device]) - float64(usage.AvailableBytes)) / seconds
		usage.Path = filesystem.Path
		*filesystem = usage
		UpdateTrend(growth, filesystem)
	}
	for groupIndex, files := range groups {
		for index := range files {
			file := &files[index]
			initial := before[groupIndex][index]
			current, err := sampleFile(file.Path)
			if err != nil {
				file.GrowthStatus = "unavailable"
				continue
			}
			file.AllocatedBytes = current.allocatedBytes
			file.ApparentBytes = current.apparentBytes
			file.ModifiedAt = current.modifiedAt
			file.identity = inodeKey{device: current.device, inode: current.inode}
			file.identityKnown = true
			if !initial.available {
				continue
			}
			if file.GrowthStatus == "replaced" || initial.metadata.device != current.device || initial.metadata.inode != current.inode {
				file.GrowthStatus = "replaced"
				continue
			}
			file.Growth = float64(current.allocatedBytes-initial.metadata.allocatedBytes) / seconds
			file.GrowthStatus = "measured"
		}
		sort.Slice(files, func(i, j int) bool {
			if files[i].Growth == files[j].Growth {
				return files[i].AllocatedBytes > files[j].AllocatedBytes
			}
			return files[i].Growth > files[j].Growth
		})
	}
	return nil
}

func UpdateTrend(growth float64, filesystem *Filesystem) {
	filesystem.GrowthBytesPerSecond = growth
	filesystem.GrowthBasis = "filesystem_available_bytes"
	filesystem.FullInSeconds = 0
	if growth > 0 {
		filesystem.FullInSeconds = float64(filesystem.AvailableBytes) / growth
	}
}

func metadataForPath(path string) (fileMetadata, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileMetadata{}, err
	}
	return metadataFromInfo(info)
}

// Sampling must not follow a pathname that was replaced with a symlink or
// treat directories and other non-regular objects as file growth.
func regularFileMetadata(path string) (fileMetadata, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileMetadata{}, err
	}
	if !info.Mode().IsRegular() {
		return fileMetadata{}, fmt.Errorf("not a regular file: %s", path)
	}
	return metadataFromInfo(info)
}

func metadataFromInfo(info os.FileInfo) (fileMetadata, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileMetadata{}, fmt.Errorf("unsupported file metadata type %T", info.Sys())
	}
	allocated := stat.Blocks * 512
	if allocated < 0 {
		allocated = 0
	}
	return fileMetadata{
		device:         uint64(stat.Dev),
		inode:          uint64(stat.Ino),
		links:          uint64(stat.Nlink),
		allocatedBytes: allocated,
		apparentBytes:  info.Size(),
		modifiedAt:     info.ModTime().UnixNano(),
	}, nil
}

func deviceOf(path string) (uint64, error) {
	metadata, err := metadataForPath(path)
	if err != nil {
		return 0, err
	}
	return metadata.device, nil
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

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
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

type fileHeap struct {
	items []File
	limit int
	less  func(File, File) bool
}

func newFileHeap(limit int, less func(File, File) bool) *fileHeap {
	result := &fileHeap{limit: limit, less: less}
	heap.Init(result)
	return result
}

func (items fileHeap) Len() int           { return len(items.items) }
func (items fileHeap) Less(i, j int) bool { return items.less(items.items[i], items.items[j]) }
func (items fileHeap) Swap(i, j int)      { items.items[i], items.items[j] = items.items[j], items.items[i] }
func (items *fileHeap) Push(value any)    { items.items = append(items.items, value.(File)) }
func (items *fileHeap) Pop() any {
	last := len(items.items) - 1
	value := items.items[last]
	items.items = items.items[:last]
	return value
}

func (items *fileHeap) add(file File) {
	if items.limit <= 0 {
		return
	}
	if items.Len() < items.limit {
		heap.Push(items, file)
		return
	}
	if items.less(items.items[0], file) {
		items.items[0] = file
		heap.Fix(items, 0)
	}
}

func (items *fileHeap) files() []File { return append([]File(nil), items.items...) }

func lessAllocated(left, right File) bool { return left.AllocatedBytes < right.AllocatedBytes }
func lessModified(left, right File) bool  { return left.ModifiedAt < right.ModifiedAt }

type trackedDirectory struct {
	Directory
	index int
}

type directoryHeap []*trackedDirectory

func (items directoryHeap) Len() int { return len(items) }
func (items directoryHeap) Less(i, j int) bool {
	return items[i].AllocatedBytes < items[j].AllocatedBytes
}
func (items directoryHeap) Swap(i, j int) {
	items[i], items[j] = items[j], items[i]
	items[i].index = i
	items[j].index = j
}
func (items *directoryHeap) Push(value any) {
	item := value.(*trackedDirectory)
	item.index = len(*items)
	*items = append(*items, item)
}
func (items *directoryHeap) Pop() any {
	old := *items
	last := len(old) - 1
	item := old[last]
	item.index = -1
	*items = old[:last]
	return item
}

type directoryTracker struct {
	limit       int
	items       directoryHeap
	byPath      map[string]*trackedDirectory
	approximate bool
}

func newDirectoryTracker(limit int) *directoryTracker {
	tracker := &directoryTracker{limit: limit, byPath: make(map[string]*trackedDirectory, limit)}
	heap.Init(&tracker.items)
	return tracker
}

func (tracker *directoryTracker) add(path string, allocatedBytes, apparentBytes int64) {
	if tracker.limit <= 0 {
		return
	}
	if item, found := tracker.byPath[path]; found {
		item.AllocatedBytes += allocatedBytes
		item.ApparentBytes += apparentBytes
		heap.Fix(&tracker.items, item.index)
		return
	}
	if len(tracker.items) < tracker.limit {
		item := &trackedDirectory{Directory: Directory{Path: path, AllocatedBytes: allocatedBytes, ApparentBytes: apparentBytes}}
		heap.Push(&tracker.items, item)
		tracker.byPath[path] = item
		return
	}
	minimum := tracker.items[0]
	delete(tracker.byPath, minimum.Path)
	minimum.Path = path
	minimum.AllocatedBytes += allocatedBytes
	minimum.ApparentBytes += apparentBytes
	minimum.Estimated = true
	tracker.byPath[path] = minimum
	heap.Fix(&tracker.items, 0)
	tracker.approximate = true
}

func (tracker *directoryTracker) result(limit int) []Directory {
	if limit <= 0 {
		return nil
	}
	result := make([]Directory, 0, len(tracker.items))
	for _, item := range tracker.items {
		result = append(result, item.Directory)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AllocatedBytes > result[j].AllocatedBytes })
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

type warningCollector struct {
	limit   int
	items   []string
	omitted int
}

func newWarningCollector(limit int) *warningCollector { return &warningCollector{limit: limit} }

func (warnings *warningCollector) add(message string) {
	if len(warnings.items) < warnings.limit {
		warnings.items = append(warnings.items, message)
		return
	}
	warnings.omitted++
}

func (warnings *warningCollector) result() []string {
	result := append([]string(nil), warnings.items...)
	if warnings.omitted > 0 {
		result = append(result, fmt.Sprintf("%d additional scan warning(s) omitted", warnings.omitted))
	}
	return result
}
