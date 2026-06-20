package config

import (
	"errors"
	"strings"
	"testing"
)

type vcfg struct {
	Listen string `koanf:"listen" validate:"required,hostname_port"`
	Mode   string `koanf:"mode" validate:"oneof=dev prod"`
}

func TestValidateConfig_TagsPass(t *testing.T) {
	if err := validateConfig(&vcfg{Listen: "127.0.0.1:8080", Mode: "prod"}); err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}
}

func TestValidateConfig_ErrorNamesDottedKey(t *testing.T) {
	// Missing required listen — error must name the koanf key "listen", not the Go field "Listen".
	err := validateConfig(&vcfg{Mode: "prod"})
	if err == nil {
		t.Fatal("expected validation error for missing listen")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error %q should name the dotted key 'listen'", err.Error())
	}
	if strings.Contains(err.Error(), "Listen") {
		t.Errorf("error %q leaked the Go field name 'Listen' instead of the koanf key", err.Error())
	}
}

func TestValidateConfig_EnumFailure(t *testing.T) {
	err := validateConfig(&vcfg{Listen: "127.0.0.1:8080", Mode: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("expected error naming 'mode', got %v", err)
	}
}

// A type implementing Validate gets its cross-field check run after tag validation.
type xcfg struct {
	A int `koanf:"a"`
	B int `koanf:"b"`
}

func (c xcfg) Validate() error {
	if c.A > c.B {
		return errors.New("a must be <= b")
	}
	return nil
}

func TestValidateConfig_RunsValidatable(t *testing.T) {
	if err := validateConfig(&xcfg{A: 2, B: 1}); err == nil || !strings.Contains(err.Error(), "a must be <= b") {
		t.Fatalf("expected cross-field error, got %v", err)
	}
	if err := validateConfig(&xcfg{A: 1, B: 2}); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
}

// Convention check: an embedded Common's Validate is reached only when the outer type's Validate
// calls it explicitly (Go method promotion would otherwise shadow it).
type cmn struct {
	Region string `koanf:"region" validate:"required"`
}

func (c cmn) Validate() error {
	if c.Region == "banned" {
		return errors.New("region banned")
	}
	return nil
}

type svc struct {
	cmn  `koanf:",squash"`
	Name string `koanf:"name"`
}

func (c svc) Validate() error {
	if err := c.cmn.Validate(); err != nil { // explicit call — the documented pattern
		return err
	}
	return nil
}

func TestValidateConfig_EmbeddedValidateCalledExplicitly(t *testing.T) {
	if err := validateConfig(&svc{cmn: cmn{Region: "banned"}, Name: "x"}); err == nil || !strings.Contains(err.Error(), "region banned") {
		t.Fatalf("expected embedded Common.Validate to run via explicit call, got %v", err)
	}
}
