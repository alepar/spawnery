package config

import (
	"testing"
	"testing/fstest"

	"github.com/knadh/koanf/providers/confmap"
)

type loadCP struct {
	LogLevel string `koanf:"log_level"`
	Listen   string `koanf:"listen"`
	Max      int    `koanf:"max"`
	Store    struct {
		DSN string `koanf:"dsn"`
	} `koanf:"store"`
	Extra string `koanf:"extra"` // only set by defaults — proves layer 0 survives
}

func loadFS() fstest.MapFS {
	return fstest.MapFS{
		"common.yaml": {Data: []byte("log_level: info\n")},
		"cp.yaml":     {Data: []byte("listen: \":8080\"\nmax: 1\nstore:\n  dsn: filedsn\n")},
	}
}

func TestLoad_FullPrecedenceChain(t *testing.T) {
	defaults := &loadCP{LogLevel: "x", Listen: "y", Max: 99, Extra: "fromdefault"}
	defaults.Store.DSN = "defdsn"

	flags := confmap.Provider(map[string]any{"listen": ":7000"}, ".") // layer 6

	cfg, err := Load[loadCP]("cp", Options{
		Args:         []string{"--env=prod"}, // no prod delta files — optional skip
		Getenv:       envFrom(map[string]string{"CP_MAX": "5"}),
		Embedded:     loadFS(),
		Defaults:     defaults,
		EnvAliases:   map[string]string{"CP_MAX": "max"},
		FlagProvider: flags,
		Sets:         []string{"store.dsn=setdsn"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Extra != "fromdefault" {
		t.Errorf("Extra = %q, want fromdefault (layer-0 default, set nowhere else)", cfg.Extra)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info (common.yaml beats default)", cfg.LogLevel)
	}
	if cfg.Listen != ":7000" {
		t.Errorf("Listen = %q, want :7000 (flag beats file)", cfg.Listen)
	}
	if cfg.Max != 5 {
		t.Errorf("Max = %d, want 5 (env beats file; string coerced to int)", cfg.Max)
	}
	if cfg.Store.DSN != "setdsn" {
		t.Errorf("Store.DSN = %q, want setdsn (--set wins over file)", cfg.Store.DSN)
	}
}

func TestLoad_MissingEnvIsFatal(t *testing.T) {
	_, err := Load[loadCP]("cp", Options{
		Args:     nil,
		Getenv:   envFrom(nil),
		Embedded: loadFS(),
	})
	if err == nil {
		t.Fatal("expected fatal error when SPAWNERY_ENV unset, got nil")
	}
}
