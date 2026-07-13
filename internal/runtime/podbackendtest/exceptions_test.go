package podbackendtest

import (
	"strings"
	"testing"
)

func TestValidateExceptions(t *testing.T) {
	cases := []string{"start_two_phase_ordering", "stop_is_idempotent"}

	t.Run("no exceptions is valid", func(t *testing.T) {
		if err := validateExceptions(cases, nil); err != nil {
			t.Fatalf("validateExceptions(nil) = %v, want nil", err)
		}
	})

	t.Run("a named exception for a known case is valid", func(t *testing.T) {
		ex := map[string]string{"stop_is_idempotent": "docker swallows the error; see #123"}
		if err := validateExceptions(cases, ex); err != nil {
			t.Fatalf("validateExceptions = %v, want nil", err)
		}
	})

	t.Run("an exception for an unknown case is an error", func(t *testing.T) {
		ex := map[string]string{"stop_is_idempotnet": "typo"}
		err := validateExceptions(cases, ex)
		if err == nil {
			t.Fatal("validateExceptions for an unknown case name = nil, want an error (a typo'd exception silently disables nothing and hides a real gap)")
		}
		if !strings.Contains(err.Error(), "stop_is_idempotnet") {
			t.Fatalf("error %q does not name the offending key", err)
		}
	})

	t.Run("an empty reason is an error", func(t *testing.T) {
		ex := map[string]string{"stop_is_idempotent": ""}
		err := validateExceptions(cases, ex)
		if err == nil {
			t.Fatal("validateExceptions with an empty reason = nil, want an error (an exception without a reason is a dropped assertion)")
		}
	})
}
