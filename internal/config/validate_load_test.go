package config

import (
	"strings"
	"testing"
	"testing/fstest"
)

type validatedCP struct {
	Listen string `koanf:"listen" validate:"required,hostname_port"`
}

func vcFS(cpBody string) fstest.MapFS {
	return fstest.MapFS{
		"common.yaml": {Data: []byte("{}\n")},
		"vc.yaml":     {Data: []byte(cpBody)},
	}
}

func TestLoad_RunsValidation_Fails(t *testing.T) {
	_, err := Load[validatedCP]("vc", Options{
		Args:     []string{"--env=dev"},
		Getenv:   envFrom(nil),
		Embedded: vcFS("other: x\n"), // listen missing
	})
	if err == nil {
		t.Fatal("expected Load to fail validation for missing required listen")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error %q should name listen", err.Error())
	}
}

func TestLoad_RunsValidation_Passes(t *testing.T) {
	cfg, err := Load[validatedCP]("vc", Options{
		Args:     []string{"--env=dev"},
		Getenv:   envFrom(nil),
		Embedded: vcFS("listen: \"127.0.0.1:8080\"\n"),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
}
