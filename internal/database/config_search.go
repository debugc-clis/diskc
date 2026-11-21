package database

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// configurationMatches searches the fixed configuration patterns without
// filepath.Glob's silent suppression of directory and metadata errors. As with
// Glob, each pattern's matches are lexical and ordinary non-directory branches
// do not match a nested wildcard. Directory symlinks are followed by Stat.
func configurationMatches(patterns []string, stat func(string) (os.FileInfo, error), readDir func(string) ([]os.DirEntry, error)) ([]string, []string) {
	result := make([]string, 0)
	warnings := make([]string, 0)
	seenPaths := make(map[string]bool)
	seenWarnings := make(map[string]bool)
	warn := func(role, path string, err error) {
		if os.IsNotExist(err) {
			return
		}
		key := role + "\x00" + path
		if seenWarnings[key] {
			return
		}
		seenWarnings[key] = true
		warnings = append(warnings, fmt.Sprintf("cannot inspect %s %s: %v; database discovery may be incomplete; verify the configuration search path and access as the affected service user", role, path, err))
	}
	type directory struct {
		entries []os.DirEntry
	}
	directories := make(map[string]directory)
	list := func(path string) []os.DirEntry {
		if cached, ok := directories[path]; ok {
			return cached.entries
		}
		directories[path] = directory{}
		info, err := stat(path)
		if err != nil {
			warn("configuration search directory", path, err)
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		entries, err := readDir(path)
		if err != nil {
			warn("configuration search directory", path, err)
		}
		// ReadDir may return a usable prefix with an error. Preserve it, but
		// make override order independent of the enumerator's entry order.
		entries = append([]os.DirEntry(nil), entries...)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		directories[path] = directory{entries: entries}
		return entries
	}
	var expand func(string) []string
	expand = func(pattern string) []string {
		if !strings.ContainsAny(pattern, "*?[\\") {
			return []string{pattern}
		}
		dir, name := filepath.Split(pattern)
		dir = filepath.Clean(dir)
		var matches []string
		for _, parent := range expand(dir) {
			for _, entry := range list(parent) {
				if matched, _ := filepath.Match(name, entry.Name()); matched {
					matches = append(matches, filepath.Join(parent, entry.Name()))
				}
			}
		}
		return matches
	}
	for _, pattern := range patterns {
		if !filepath.IsAbs(pattern) {
			continue
		}
		if _, err := filepath.Match(pattern, ""); err != nil {
			warn("configuration pattern", pattern, err)
			continue
		}
		matched := expand(pattern)
		sort.Strings(matched)
		for _, path := range matched {
			if seenPaths[path] {
				continue
			}
			seenPaths[path] = true
			if _, err := stat(path); err != nil {
				warn("configuration candidate", path, err)
				continue
			}
			result = append(result, path)
		}
	}
	return result, warnings
}
