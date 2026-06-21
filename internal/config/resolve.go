package config

import (
	"fmt"
	"strings"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// parseRef recognizes a whole-value reference of the form ${scheme:arg}. Only the entire (trimmed)
// value may be a reference — embedded interpolation like "prefix-${env:X}" is left literal — which
// keeps resolution unambiguous. scheme must be one or more letters; arg is everything up to the
// final '}'.
func parseRef(s string) (scheme, arg string, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return "", "", false
	}
	body := s[2 : len(s)-1]
	colon := strings.IndexByte(body, ':')
	if colon <= 0 {
		return "", "", false
	}
	scheme = body[:colon]
	for _, r := range scheme {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return "", "", false
		}
	}
	return scheme, body[colon+1:], true
}

// resolveRefs walks every string leaf of the merged config, resolves any whole-value
// ${scheme:arg} reference through the registered resolvers, and overlays the cleartext as a top
// layer. It runs after all layers merge and before decode, so the resolved value is coerced to the
// field type like any other (a ${env:PORT} can feed an int field). Fail-closed: an unknown scheme
// or a resolver error aborts the load.
func resolveRefs(k *koanf.Koanf, resolvers map[string]Resolver) error {
	overrides := map[string]any{}
	for key, val := range k.All() {
		switch v := val.(type) {
		case string:
			resolved, ok, err := resolveValue(key, v, resolvers)
			if err != nil {
				return err
			}
			if ok {
				overrides[key] = resolved
			}
		case []any:
			// koanf flattens a list into a single leaf, so list elements (e.g. a list of
			// ${sops:...} secrets) must be walked here rather than via k.All() keys.
			out := make([]any, len(v))
			changed := false
			for i, el := range v {
				out[i] = el
				s, ok := el.(string)
				if !ok {
					continue
				}
				resolved, ok, err := resolveValue(key, s, resolvers)
				if err != nil {
					return err
				}
				if ok {
					out[i] = resolved
					changed = true
				}
			}
			if changed {
				overrides[key] = out
			}
		}
	}
	if len(overrides) > 0 {
		if err := k.Load(confmap.Provider(overrides, "."), nil); err != nil {
			return fmt.Errorf("applying resolved references: %w", err)
		}
	}
	return nil
}

// resolveValue resolves a single string value if it is a whole-value ${scheme:arg} reference.
// Returns (resolved, true, nil) when it was a reference, (\"\", false, nil) when it was not, and a
// fail-closed error on an unknown scheme or a resolver failure.
func resolveValue(key, s string, resolvers map[string]Resolver) (string, bool, error) {
	scheme, arg, isRef := parseRef(s)
	if !isRef {
		return "", false, nil
	}
	r, ok := resolvers[scheme]
	if !ok {
		return "", false, fmt.Errorf("config key %q: unknown reference scheme %q in %q", key, scheme, s)
	}
	resolved, err := r.Resolve(arg)
	if err != nil {
		return "", false, fmt.Errorf("config key %q: resolving ${%s:%s}: %w", key, scheme, arg, err)
	}
	return resolved, true, nil
}
