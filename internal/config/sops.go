package config

import (
	"fmt"
	"sync"

	"github.com/getsops/sops/v3/decrypt"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

// sopsResolver resolves ${sops:dotted.key} from a SOPS-encrypted YAML document (typically the
// //go:embed'd secrets.<env>.sops.yaml). The ciphertext is decrypted in-process exactly once and
// the cleartext map is cached for the process lifetime.
//
// The age identity (secret-zero) is supplied out-of-band via SOPS_AGE_KEY_FILE / SOPS_AGE_KEY,
// which decrypt.Data reads itself; it never appears in config. A cloud-KMS recipient on the same
// file is unwrapped automatically when present.
type sopsResolver struct {
	ciphertext []byte
	once       sync.Once
	k          *koanf.Koanf
	err        error
}

func newSopsResolver(ciphertext []byte) *sopsResolver {
	return &sopsResolver{ciphertext: ciphertext}
}

// NewSopsResolver builds a ${sops:} resolver over a SOPS-encrypted YAML document (e.g. an
// //go:embed'd secrets file), for binaries that wire the resolver explicitly.
func NewSopsResolver(ciphertext []byte) Resolver { return newSopsResolver(ciphertext) }

func (*sopsResolver) Scheme() string { return "sops" }

func (r *sopsResolver) Resolve(key string) (string, error) {
	r.once.Do(r.decrypt)
	if r.err != nil {
		return "", r.err
	}
	if !r.k.Exists(key) {
		return "", fmt.Errorf("sops secret %q not found in the encrypted secrets file", key)
	}
	return r.k.String(key), nil
}

func (r *sopsResolver) decrypt() {
	plain, err := decrypt.Data(r.ciphertext, "yaml")
	if err != nil {
		r.err = fmt.Errorf("decrypting sops secrets (is SOPS_AGE_KEY_FILE set?): %w", err)
		return
	}
	k := koanf.New(".")
	if err := k.Load(rawbytes.Provider(plain), yaml.Parser()); err != nil {
		r.err = fmt.Errorf("parsing decrypted sops secrets: %w", err)
		return
	}
	r.k = k
}
