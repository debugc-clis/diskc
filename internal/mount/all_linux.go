//go:build linux

package mount

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func All() ([]string, error) {
	mounts, err := readLinuxMounts()
	if err != nil {
		return nil, err
	}
	seenDevices := make(map[string]bool)
	paths := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Kind == KindRemote {
			continue
		}
		if seenDevices[mount.identity] {
			continue
		}
		seenDevices[mount.identity] = true
		paths = append(paths, mount.Path)
	}
	sort.Strings(paths)
	return paths, nil
}

func List() ([]Info, error) {
	mounts, err := readLinuxMounts()
	if err != nil {
		return nil, err
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Path < mounts[j].Path })
	return mounts, nil
}

func readLinuxMounts() ([]Info, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	defer file.Close()
	return parseLinuxMounts(file)
}

func parseLinuxMounts(reader io.Reader) ([]Info, error) {
	seen := make(map[string]bool)
	mounts := make([]Info, 0)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), " - ", 2)
		if len(fields) != 2 {
			continue
		}
		pre := strings.Fields(fields[0])
		post := strings.Fields(fields[1])
		if len(pre) < 6 || len(post) < 2 {
			continue
		}
		kind := Classify(post[0])
		if kind == KindPseudo || kind == KindMemory {
			continue
		}
		path := unescape(pre[4])
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		readOnly := hasOption(pre[5], "ro")
		if len(post) > 2 {
			readOnly = readOnly || hasOption(post[2], "ro")
		}
		mounts = append(mounts, Info{
			Source:   unescape(post[1]),
			Path:     path,
			Type:     post[0],
			Kind:     kind,
			ReadOnly: readOnly,
			identity: pre[2],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}
