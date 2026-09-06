//go:build linux

package mount

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

func All() ([]string, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	defer file.Close()
	seen := make(map[string]bool)
	seenDevices := make(map[string]bool)
	paths := make([]string, 0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), " - ", 2)
		if len(fields) != 2 {
			continue
		}
		pre := strings.Fields(fields[0])
		post := strings.Fields(fields[1])
		if len(pre) < 5 || len(post) == 0 || virtualFilesystems[post[0]] || seen[pre[4]] || seenDevices[pre[2]] {
			continue
		}
		path := unescape(pre[4])
		seen[path] = true
		seenDevices[pre[2]] = true
		paths = append(paths, path)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}
