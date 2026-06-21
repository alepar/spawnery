package config

import (
	"flag"
	"strings"
)

// multiFlag is a repeatable string flag, used for --set key.path=value.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// StdFlags parses the standard server-binary flags (--env, --config-dir, --set) from args and
// returns the external config dir and the --set overrides, to pass to Load. --env is registered
// so the flag parser accepts it, but it is consumed by Load's bootstrap (which scans Args). Uses
// flag.ExitOnError, matching the server binaries' loadConfig behavior. (The urfave-based CLIs do
// not use this — they wire flags through cliflagv3/confmap.)
func StdFlags(name string, args []string) (configDir string, sets []string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var m multiFlag
	_ = fs.String("env", "", "environment dev|staging|prod (overrides SPAWNERY_ENV)")
	dir := fs.String("config-dir", "", "external config override dir (overrides SPAWNERY_CONFIG_DIR)")
	fs.Var(&m, "set", "override a config key: key.path=value (repeatable)")
	_ = fs.Parse(args)
	return *dir, []string(m)
}
