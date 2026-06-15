package storage

import (
	"errors"
	"fmt"
	"strings"
)

var ErrUnsupportedBackend = errors.New("storage: unsupported backend")

type UnsupportedBackendError struct {
	BackendURI string
	Scheme     string
}

func (e *UnsupportedBackendError) Error() string {
	if e == nil {
		return ErrUnsupportedBackend.Error()
	}
	if e.Scheme == "" {
		return ErrUnsupportedBackend.Error()
	}
	if e.BackendURI == "" {
		return fmt.Sprintf("%s: %s", ErrUnsupportedBackend, e.Scheme)
	}
	return fmt.Sprintf("%s: %s (%s)", ErrUnsupportedBackend, e.Scheme, e.BackendURI)
}

func (e *UnsupportedBackendError) Unwrap() error { return ErrUnsupportedBackend }

type BackendResolver interface {
	Resolve(backendURI string) (Backend, error)
}

type SchemeResolver struct {
	scratchRoot string
}

func NewSchemeResolver(scratchRoot string) *SchemeResolver {
	return &SchemeResolver{scratchRoot: scratchRoot}
}

func (r *SchemeResolver) Resolve(backendURI string) (Backend, error) {
	scheme, _, hasScheme := strings.Cut(backendURI, ":")
	if !hasScheme || scheme == "" || scheme == "scratch" {
		return NewScratch(r.scratchRoot), nil
	}
	return nil, &UnsupportedBackendError{BackendURI: backendURI, Scheme: scheme}
}
