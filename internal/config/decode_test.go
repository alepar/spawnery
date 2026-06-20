package config

import (
	"strings"
	"testing"
	"time"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

type decodeSample struct {
	Port    int           `koanf:"port"`
	Timeout time.Duration `koanf:"timeout"`
	Debug   bool          `koanf:"debug"`
}

func kFromFlat(m map[string]any) *koanf.Koanf {
	k := koanf.New(".")
	_ = k.Load(confmap.Provider(m, "."), nil)
	return k
}

func TestDecodeInto_CoercesStringScalars(t *testing.T) {
	// All values arrive as strings (as the env / --set layers produce them).
	k := kFromFlat(map[string]any{"port": "9090", "timeout": "5s", "debug": "true"})
	var got decodeSample
	if err := decodeInto(k, &got); err != nil {
		t.Fatalf("decodeInto: %v", err)
	}
	if got.Port != 9090 {
		t.Errorf("Port = %d, want 9090 (string->int via WeaklyTypedInput)", got.Port)
	}
	if got.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s (string->Duration via hook)", got.Timeout)
	}
	if !got.Debug {
		t.Errorf("Debug = %v, want true (string->bool)", got.Debug)
	}
}

func TestDecodeInto_UncoercibleIsError(t *testing.T) {
	k := kFromFlat(map[string]any{"port": "abc"})
	var got decodeSample
	err := decodeInto(k, &got)
	if err == nil {
		t.Fatal("expected decode error for port=abc, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "port") {
		t.Errorf("error %q should name the offending field 'port'", err.Error())
	}
}
