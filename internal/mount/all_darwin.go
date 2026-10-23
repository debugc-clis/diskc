//go:build darwin

package mount

import (
	"sort"
	"strings"
	"syscall"
)

func All() ([]string, error) {
	mounts, err := readDarwinMounts()
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Kind == KindRemote {
			continue
		}
		paths = append(paths, mount.Path)
	}
	sort.Strings(paths)
	return paths, nil
}

func List() ([]Info, error) {
	mounts, err := readDarwinMounts()
	if err != nil {
		return nil, err
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Path < mounts[j].Path })
	return mounts, nil
}

func readDarwinMounts() ([]Info, error) {
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
	if count > len(stats) {
		count = len(stats)
	}
	seen := make(map[string]bool)
	mounts := make([]Info, 0, count)
	for _, stat := range stats[:count] {
		filesystemType := int8String(stat.Fstypename[:])
		kind := Classify(filesystemType)
		path := int8String(stat.Mntonname[:])
		if path == "" || kind == KindPseudo || kind == KindMemory || darwinInternalMount(path) || strings.HasPrefix(path, "/Library/Developer/CoreSimulator/") || seen[path] {
			continue
		}
		seen[path] = true
		mounts = append(mounts, Info{
			Source:   int8String(stat.Mntfromname[:]),
			Path:     path,
			Type:     filesystemType,
			Kind:     kind,
			ReadOnly: stat.Flags&1 != 0,
			identity: path,
		})
	}
	return mounts, nil
}

func darwinInternalMount(path string) bool {
	return strings.HasPrefix(path, "/System/Volumes/") && path != "/System/Volumes/Data"
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
