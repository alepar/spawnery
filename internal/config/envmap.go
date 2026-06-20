package config

// buildEnvLayer maps the explicitly-aliased environment variables that are currently set into a
// flat dotted-key → value map (layer 5), suitable for a koanf confmap.Provider with "." delimiter.
//
// The mapping is an explicit name→key table (not prefix auto-derivation): koanf's env provider
// treats "_" as the nesting delimiter, which would mangle underscore leaf names like
// store.max_conns; an explicit table sidesteps that and also covers legacy vars with no common
// prefix. Only vars that are actually set contribute, so an unset var never clobbers a lower layer.
func buildEnvLayer(aliases map[string]string, getenv func(string) (string, bool)) map[string]any {
	out := make(map[string]any)
	for envName, key := range aliases {
		if v, ok := getenv(envName); ok {
			out[key] = v
		}
	}
	return out
}
