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
		s, ok := val.(string)
		if !ok {
			continue
		}
		scheme, arg, isRef := parseRef(s)
		if !isRef {
			continue
		}
		r, ok := resolvers[scheme]
		if !ok {
			return fmt.Errorf("config key %q: unknown reference scheme %q in %q", key, scheme, s)
		}
		resolved, err := r.Resolve(arg)
		if err != nil {
			return fmt.Errorf("config key %q: resolving ${%s:%s}: %w", key, scheme, arg, err)
		}
		overrides[key] = resolved
	}
	if len(overrides) > 0 {
		if err := k.Load(confmap.Provider(overrides, "."), nil); err != nil {
			return fmt.Errorf("applying resolved references: %w", err)
		}
	}
	return nil
}
