package proc

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
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

func AttributeWriters(files []disk.File) {
	byIdentity := make(map[identity][]int)
	for index := range files {
		files[index].Writers = nil
		device, inode, known := files[index].Identity()
		if known {
			id := identity{device: device, inode: inode}
			byIdentity[id] = append(byIdentity[id], index)
		}
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return
	}
	seen := make(map[identity]map[int]bool)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(procRoot, entry.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if !writableDescriptor(pid, fd.Name()) {
				continue
			}
			id, err := fileIdentity(filepath.Join(procRoot, entry.Name(), "fd", fd.Name()))
			if err != nil {
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
}

func DeletedFiles() []DeletedFile {
	type retained struct {
		file    DeletedFile
		writers map[int]struct{}
	}
	files := make(map[identity]*retained)
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(procRoot, entry.Name(), "fd"))
		if err != nil {
			continue
		}
		writer := writerInfo(pid)
		for _, fd := range fds {
			fdPath := filepath.Join(procRoot, entry.Name(), "fd", fd.Name())
			target, err := os.Readlink(fdPath)
			if err != nil || !strings.HasSuffix(target, " (deleted)") {
				continue
			}
			id, err := fileIdentity(fdPath)
			if err != nil {
				continue
			}
			item := files[id]
			if item == nil {
				info, statErr := os.Stat(fdPath)
				if statErr != nil {
					continue
				}
				stat, ok := info.Sys().(*syscall.Stat_t)
				if !ok {
					continue
				}
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
	return result
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

func writableDescriptor(pid int, fd string) bool {
	file, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "fdinfo", fd))
	if err != nil {
		return false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "flags:" {
			continue
		}
		flags, err := strconv.ParseUint(fields[1], 8, 32)
		return err == nil && flags&3 != 0
	}
	return false
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
