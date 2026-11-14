// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"strings"
)

// readINI returns the keys of one section of a simple INI file, as written by
// the Windows installer: "[section]" lines and "key=value" lines.
func readINI(path, section string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	cur := ""
	text := strings.TrimPrefix(strings.ReplaceAll(string(b), "\r\n", "\n"), "\ufeff")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}
		if line[0] == '[' && strings.HasSuffix(line, "]") {
			cur = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		if cur != section {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return out, nil
}
