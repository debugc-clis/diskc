package proc

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/debugc-clis/diskc/internal/disk"
)

type DeletedFile struct {
	Path           string        `json:"path"`
	AllocatedBytes int64         `json:"allocated_bytes"`
	ApparentBytes  int64         `json:"apparent_bytes"`
	Writers        []disk.Writer `json:"writers"`
	Device         uint64        `json:"-"`
}

var procRoot = "/proc"

func AttributeWriters(files []disk.File) []string {
	byIdentity := make(map[identity][]int)
	for index := range files {
		files[index].Writers = nil
		device, inode, known := files[index].Identity()
		if known {
			id := identity{device: device, inode: inode}
			byIdentity[id] = append(byIdentity[id], index)
		}
	}
	if !inspectProc() {
		return nil
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return unavailableWarning("writer attribution", "an empty writers result does not establish that no process writes the files")
	}
	var failures inspectionFailures
	seen := make(map[identity]map[int]bool)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(procRoot, entry.Name(), "fd"))
		if err != nil {
			failures.record(&failures.directories, err)
			continue
		}
		for _, fd := range fds {
			writable, err := writableDescriptor(pid, fd.Name())
			if err != nil {
				failures.record(&failures.fdinfo, err)
				continue
			}
			if !writable {
				continue
			}
			id, err := fileIdentity(filepath.Join(procRoot, entry.Name(), "fd", fd.Name()))
			if err != nil {
				failures.record(&failures.descriptors, err)
				continue
			}
			indices := byIdentity[id]
			if len(indices) == 0 || seen[id][pid] {
				continue
			}
			writer := writerInfo(pid)
			for _, fileIndex := range indices {
				files[fileIndex].Writers = append(files[fileIndex].Writers, writer)
			}
			if seen[id] == nil {
				seen[id] = make(map[int]bool)
			}
			seen[id][pid] = true
		}
	}
	return failures.warnings("writer attribution", "an empty writers result does not establish that no process writes the files")
}

func DeletedFiles() ([]DeletedFile, []string) {
	type retained struct {
		file    DeletedFile
		writers map[int]struct{}
	}
	files := make(map[identity]*retained)
	if !inspectProc() {
		return nil, nil
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, unavailableWarning("deleted-open inspection", "retained-space results are not exhaustive")
	}
	var failures inspectionFailures
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(procRoot, entry.Name(), "fd"))
		if err != nil {
			failures.record(&failures.directories, err)
			continue
		}
		writer := writerInfo(pid)
		for _, fd := range fds {
			fdPath := filepath.Join(procRoot, entry.Name(), "fd", fd.Name())
			target, err := os.Readlink(fdPath)
			if err != nil {
				failures.record(&failures.descriptors, err)
				continue
			}
			if !strings.HasSuffix(target, " (deleted)") {
				continue
			}
			info, err := os.Stat(fdPath)
			if err != nil {
				failures.record(&failures.descriptors, err)
				continue
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				failures.descriptors++
				continue
			}
			// The suffix alone is ambiguous: a linked file may literally have
			// that name. Use this one observation for unlink status, identity,
			// and size, and exclude non-regular objects such as deleted dirs.
			if !info.Mode().IsRegular() || stat.Nlink != 0 {
				continue
			}
			id := identity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
			item := files[id]
			if item == nil {
				allocatedBytes := stat.Blocks * 512
				if allocatedBytes < 0 {
					allocatedBytes = 0
				}
				item = &retained{
					file: DeletedFile{
						Path:           strings.TrimSuffix(target, " (deleted)"),
						AllocatedBytes: allocatedBytes,
						ApparentBytes:  info.Size(),
						Device:         id.device,
					},
					writers: make(map[int]struct{}),
				}
				files[id] = item
			}
			if _, found := item.writers[pid]; !found {
				item.file.Writers = append(item.file.Writers, writer)
				item.writers[pid] = struct{}{}
			}
		}
	}
	result := make([]DeletedFile, 0, len(files))
	for _, item := range files {
		result = append(result, item.file)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AllocatedBytes > result[j].AllocatedBytes })
	return result, failures.warnings("deleted-open inspection", "retained-space results are not exhaustive")
}

// A fake proc root is used by portable fixtures. Real process descriptor
// inspection is only supported on Linux.
func inspectProc() bool {
	return runtime.GOOS == "linux" || procRoot != "/proc"
}

const inspectionNextStep = "run as the affected service user when practical and verify process visibility"

func unavailableWarning(operation, consequence string) []string {
	return []string{fmt.Sprintf("Linux %s unavailable: cannot read /proc; %s; %s", operation, consequence, inspectionNextStep)}
}

type inspectionFailures struct {
	directories int
	fdinfo      int
	descriptors int
}

func (failures *inspectionFailures) record(counter *int, err error) {
	// PIDs and descriptors routinely disappear during a live inspection.
	if !os.IsNotExist(err) && !errors.Is(err, syscall.ESRCH) {
		(*counter)++
	}
}

func (failures inspectionFailures) warnings(operation, consequence string) []string {
	if failures.directories+failures.fdinfo+failures.descriptors == 0 {
		return nil
	}
	return []string{fmt.Sprintf("Linux %s partial: could not inspect %d process fd directories, %d fdinfo records, %d descriptors; %s; %s", operation, failures.directories, failures.fdinfo, failures.descriptors, consequence, inspectionNextStep)}
}

type identity struct {
	device uint64
	inode  uint64
}

func fileIdentity(path string) (identity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return identity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return identity{}, fmt.Errorf("unsupported file metadata")
	}
	return identity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}

func writerInfo(pid int) disk.Writer {
	name := readFirstLine(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	exe, _ := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "exe"))
	uid := processUID(pid)
	return disk.Writer{PID: pid, Process: name, Exe: exe, UID: uid, Service: serviceName(pid), Container: containerID(pid)}
}

func serviceName(pid int) string {
	file, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		for _, part := range strings.Split(scanner.Text(), "/") {
			if strings.HasSuffix(part, ".service") {
				return part
			}
		}
	}
	return ""
}

func containerID(pid int) string {
	file, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		for _, part := range strings.Split(scanner.Text(), "/") {
			candidate := strings.TrimSuffix(part, ".scope")
			candidate = strings.TrimPrefix(candidate, "docker-")
			candidate = strings.TrimPrefix(candidate, "cri-containerd-")
			candidate = strings.TrimPrefix(candidate, "crio-")
			if len(candidate) >= 12 && isHex(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func isHex(value string) bool {
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') && !(character >= 'A' && character <= 'F') {
			return false
		}
	}
	return true
}

func writableDescriptor(pid int, fd string) (bool, error) {
	file, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "fdinfo", fd))
	if err != nil {
		return false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "flags:" {
			continue
		}
		flags, err := strconv.ParseUint(fields[1], 8, 32)
		if err != nil {
			return false, fmt.Errorf("invalid descriptor flags: %w", err)
		}
		return flags&3 != 0, nil
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, fmt.Errorf("descriptor flags unavailable")
}

func processUID(pid int) string {
	file, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return "?"
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "Uid:" {
			return fields[1]
		}
	}
	return "?"
}

func readFirstLine(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "?"
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		return scanner.Text()
	}
	return "?"
}
