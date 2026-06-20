package config

import (
	"fmt"
	"testing"
	"testing/fstest"
)

type secretCP struct {
	Token Secret `koanf:"token"`
}

func secretFS(tokenLine string) fstest.MapFS {
	return fstest.MapFS{
		"common.yaml": {Data: []byte("{}\n")},
		"cp.yaml":     {Data: []byte(tokenLine + "\n")},
	}
}

func TestLoad_ResolvesReferenceIntoRedactedSecret(t *testing.T) {
	cfg, err := Load[secretCP]("cp", Options{
		Args:     []string{"--env=dev"},
		Getenv:   envFrom(map[string]string{"TOK": "realsecret"}),
		Embedded: secretFS("token: ${env:TOK}"),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(cfg.Token) != "realsecret" {
		t.Errorf("Token value = %q, want realsecret (resolved before decode)", string(cfg.Token))
	}
	if got := fmt.Sprintf("%v", cfg.Token); got != "***" {
		t.Errorf("Token render = %q, want *** (redacted)", got)
	}
}

func TestLoad_UnresolvableReferenceIsFatal(t *testing.T) {
	_, err := Load[secretCP]("cp", Options{
		Args:     []string{"--env=dev"},
		Getenv:   envFrom(nil), // TOK not set
		Embedded: secretFS("token: ${env:TOK}"),
	})
	if err == nil {
		t.Fatal("expected fatal error for unresolvable ${env:TOK}, got nil")
	}
}
