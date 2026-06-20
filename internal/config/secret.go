package config

import (
	"fmt"
	"io"
)

// redacted is what a Secret renders as in any human- or machine-readable output.
const redacted = "***"

// Secret is a string that always renders as "***" — in fmt verbs (%v/%s/%+v/%#v), JSON, and
// YAML. Redaction is type-level rather than provenance-tracked, so a secret cannot leak through a
// stray %+v, an error wrap, a panic, or a third-party logger. Code reads the real value with
// string(s); only formatting/marshaling is redacted.
type Secret string

// Format renders the secret as "***" for every fmt verb, so the value never leaks via any
// formatting directive (including when nested in a struct printed with %+v / %#v).
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// String redacts for any code that calls .String() directly.
func (Secret) String() string { return redacted }

// GoString redacts for %#v paths that bypass Format.
func (Secret) GoString() string { return redacted }

// MarshalJSON redacts when a config struct is JSON-encoded.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalYAML redacts when a config struct is YAML-encoded.
func (Secret) MarshalYAML() (any, error) { return redacted, nil }
