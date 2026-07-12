package mtls

import (
	"errors"
	"fmt"
	"net/http"

	"spawnery/internal/pki"
)

const (
	roleAnonymous      = "anonymous"
	roleServiceCP      = "service:cp"
	roleServiceAuthsvc = "service:authsvc"
	roleNodeCloud      = "node:cloud"
	roleNodeSelfHosted = "node:self-hosted"
)

// Policy maps canonical principal roles to the exact operations they may invoke.
type Policy map[string]map[string]struct{}

// Authorize permits only an explicitly registered role-operation pair.
func (p Policy) Authorize(operation string, principal *pki.Principal) error {
	role, err := policyRole(principal)
	if err != nil {
		return err
	}
	if operation == "" {
		return errors.New("mtls: empty operation is not authorized")
	}
	operations, ok := p[role]
	if !ok {
		return fmt.Errorf("mtls: role %q is not authorized", role)
	}
	if _, ok := operations[operation]; !ok {
		return fmt.Errorf("mtls: role %q is not authorized for operation %q", role, operation)
	}
	return nil
}

// HTTPMiddleware enforces policy for the exact operation returned for each request.
func (p Policy) HTTPMiddleware(operation func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var name string
		if operation != nil {
			name = operation(r)
		}
		principal, ok := PrincipalFromContext(r.Context())
		if !ok {
			if err := p.Authorize(name, nil); err != nil {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		} else if err := p.Authorize(name, &principal); err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func policyRole(principal *pki.Principal) (string, error) {
	if principal == nil {
		return roleAnonymous, nil
	}
	switch {
	case principal.Kind == pki.KindService && principal.Role == pki.RoleCP:
		if principal.AccountID != "" || principal.NodeID != "" {
			break
		}
		if _, err := pki.ServiceID(principal.TrustDomain, principal.Role, principal.InstanceID); err == nil {
			return roleServiceCP, nil
		}
	case principal.Kind == pki.KindService && principal.Role == pki.RoleAuthService:
		if principal.AccountID != "" || principal.NodeID != "" {
			break
		}
		if _, err := pki.ServiceID(principal.TrustDomain, principal.Role, principal.InstanceID); err == nil {
			return roleServiceAuthsvc, nil
		}
	case principal.Kind == pki.KindNode && principal.Role == pki.RoleCloud:
		if principal.InstanceID != "" {
			break
		}
		if _, err := pki.NodeID(principal.TrustDomain, principal.Role, principal.AccountID, principal.NodeID); err == nil {
			return roleNodeCloud, nil
		}
	case principal.Kind == pki.KindNode && principal.Role == pki.RoleSelfHosted:
		if principal.InstanceID != "" {
			break
		}
		if _, err := pki.NodeID(principal.TrustDomain, principal.Role, principal.AccountID, principal.NodeID); err == nil {
			return roleNodeSelfHosted, nil
		}
	}
	return "", fmt.Errorf("mtls: invalid principal kind %q role %q", principal.Kind, principal.Role)
}
