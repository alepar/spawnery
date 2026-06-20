package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
)

// Validatable is implemented by config types that need cross-field validation beyond struct tags.
// Because Go method promotion lets an outer type's Validate shadow an embedded Common.Validate,
// the outer Validate MUST call the embedded one explicitly.
type Validatable interface {
	Validate() error
}

// validate is the shared validator, configured to report the dotted koanf key (not the Go field
// name) in error namespaces.
var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New()
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("koanf"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
	return v
}

// validateConfig runs format-only struct-tag validation (required/oneof/ranges/formats), then any
// cross-field Validate() the type implements. It is fail-fast and reports the offending dotted
// key. Path existence/permission checks are intentionally NOT done here (they belong to the owning
// component at use time), keeping validation hermetic.
func validateConfig(v any) error {
	if err := validate.Struct(v); err != nil {
		var verrs validator.ValidationErrors
		if errors.As(err, &verrs) {
			msgs := make([]string, 0, len(verrs))
			for _, fe := range verrs {
				msgs = append(msgs, fmt.Sprintf("%s (failed %q)", keyPath(fe.Namespace()), fe.Tag()))
			}
			return fmt.Errorf("config validation failed: %s", strings.Join(msgs, "; "))
		}
		return err
	}
	if val, ok := v.(Validatable); ok {
		if err := val.Validate(); err != nil {
			return fmt.Errorf("config validation failed: %w", err)
		}
	}
	return nil
}

// keyPath strips the root struct name from a validator namespace, leaving the dotted key path
// relative to the binary's config (e.g. "CP.store.dsn" -> "store.dsn").
func keyPath(namespace string) string {
	if i := strings.IndexByte(namespace, '.'); i >= 0 {
		return namespace[i+1:]
	}
	return namespace
}
