package config

import (
	"fmt"
	"strings"
)

// parseSets turns repeated --set key.path=value strings into a flat map of dotted-key → string
// value, suitable for a koanf confmap.Provider with "." delimiter (it unflattens the keys).
//
// Values are kept as strings (no YAML scalar coercion), so `--set listen=:8080` stays ":8080";
// the single decode pass coerces to the schema type. Only the first '=' splits, so values may
// contain '='. Scalar-only: lists/maps are set via files, not --set.
func parseSets(sets []string) (map[string]any, error) {
	out := make(map[string]any, len(sets))
	for _, s := range sets {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, fmt.Errorf("--set %q: missing '=' (expected key.path=value)", s)
		}
		key := s[:eq]
		if key == "" {
			return nil, fmt.Errorf("--set %q: empty key", s)
		}
		out[key] = s[eq+1:]
	}
	return out, nil
}
