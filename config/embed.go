// Package configfiles holds the committed, layered configuration files for the spawnery binaries
// and embeds them, so every binary carries its full baseline regardless of working directory or
// container layout. The secrets.<env>.sops.yaml files are ciphertext (SOPS+age); the age identity
// that decrypts them is delivered out-of-band (SOPS_AGE_KEY_FILE), never embedded.
package configfiles

import "embed"

// FS is the embedded config tree: common.yaml, <svc>.yaml, <svc>.<env>.yaml, secrets.*.sops.yaml.
//
//go:embed *.yaml
var FS embed.FS
