//go:build darwin

package mount

import (
	"sort"
	"strings"
	"syscall"
)

func All() ([]string, error) {
	const mntNowait = 2
	count, err := syscall.Getfsstat(nil, mntNowait)
	if err != nil {
		return nil, err
	}
	stats := make([]syscall.Statfs_t, count)
	count, err = syscall.Getfsstat(stats, mntNowait)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	paths := make([]string, 0, count)
	for _, stat := range stats[:count] {
		filesystemType := int8String(stat.Fstypename[:])
		path := int8String(stat.Mntonname[:])
		if path == "" || virtualFilesystems[filesystemType] || strings.HasPrefix(path, "/Library/Developer/CoreSimulator/") || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func int8String(value []int8) string {
	bytes := make([]byte, 0, len(value))
	for _, character := range value {
		if character == 0 {
			break
		}
		bytes = append(bytes, byte(character))
	}
	return string(bytes)
}
