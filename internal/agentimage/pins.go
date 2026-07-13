// Package agentimage asserts facts about the built agent image (spawnery/agent:dev).
// The Dockerfile's ARG defaults are the single source of truth for every agent binary version;
// this package parses them and (under -tags e2e) proves the image's actual binaries match.
package agentimage

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
)

// argRe matches a Dockerfile `ARG NAME` or `ARG NAME=value` line. An ARG without a default has
// no capture group 2 (empty string), which callers must treat as "not pinned".
var argRe = regexp.MustCompile(`^\s*ARG\s+([A-Za-z0-9_]+)(?:=(\S+))?\s*$`)

// Pins parses `ARG <NAME>=<value>` defaults out of the Dockerfile at dockerfilePath. An ARG
// declared without a default (`ARG NAME`, no `=`) is recorded with an empty value.
func Pins(dockerfilePath string) (map[string]string, error) {
	f, err := os.Open(dockerfilePath)
	if err != nil {
		return nil, fmt.Errorf("agentimage: open %s: %w", dockerfilePath, err)
	}
	defer f.Close()

	pins := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := argRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		pins[m[1]] = m[2]
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("agentimage: scan %s: %w", dockerfilePath, err)
	}
	return pins, nil
}

// semverRe extracts the first dotted-triple numeric version token from a string.
var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// SemverOf extracts the first `\d+\.\d+\.\d+` token from s, or "" if none is present. It
// normalizes both sides of a version comparison: a Dockerfile ARG default such as
// "rust-v0.137.0", "v1.41.0", or "2.1.197-1" and a binary's `--version` output alike.
func SemverOf(s string) string {
	return semverRe.FindString(s)
}
