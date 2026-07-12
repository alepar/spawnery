package authsvc

import (
	"net/http"

	"spawnery/gen/auth/v1/authv1connect"
	"spawnery/internal/mtls"
)

const (
	operationEnroll          = "authsvc.enroll"
	operationNodeRevocations = "authsvc.node-revocations"
	operationCredentialMint  = "authsvc.credential-mint"
	operationRevocations     = "authsvc.revocations"
	operationGitHubLink      = "authsvc.github-link-status"
)

// DefaultInternalPolicy is the complete AS internal route/principal matrix.
func DefaultInternalPolicy() mtls.Policy {
	return mtls.Policy{
		"anonymous":        {operationEnroll: {}},
		"node:cloud":       {operationNodeRevocations: {}, operationCredentialMint: {}},
		"node:self-hosted": {operationNodeRevocations: {}, operationCredentialMint: {}},
		"service:cp":       {operationRevocations: {}, operationGitHubLink: {}},
	}
}

// Handler is retained as the public-handler compatibility entry point.
func (s *Service) Handler() http.Handler { return s.PublicHandler() }

// PublicHandler returns only browser and CLI identity routes.
func (s *Service) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /ca/root", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		_, _ = w.Write(s.RootCAPEM())
	})

	if s.githubLinkExchanger != nil {
		ghNoop := func(http.ResponseWriter, *http.Request) {}
		mux.HandleFunc("POST /github/link/start", s.ghLinkCORSBearerSimple(s.serveGitHubLinkStart))
		mux.HandleFunc("OPTIONS /github/link/start", s.ghLinkCORSBearerSimple(ghNoop))
		mux.HandleFunc("GET /github/link/callback", s.serveGitHubLinkCallback)
		mux.HandleFunc("POST /github/link/redeem", s.ghLinkCORSCredentialed(s.serveGitHubLinkRedeem))
		mux.HandleFunc("OPTIONS /github/link/redeem", s.ghLinkCORSCredentialed(ghNoop))
		mux.HandleFunc("GET /github/links", s.ghLinkCORSBearerSimple(s.serveGitHubLinkList))
		mux.HandleFunc("OPTIONS /github/links", s.ghLinkCORSBearerSimple(ghNoop))
		mux.HandleFunc("POST /github/link/revoke", s.ghLinkCORSBearerSimple(s.serveGitHubLinkRevoke))
		mux.HandleFunc("OPTIONS /github/link/revoke", s.ghLinkCORSBearerSimple(ghNoop))
	}

	if s.deviceSet != nil {
		ds := s.deviceSet
		mux.HandleFunc("POST /devices/append", ds.corsBearerSimple(ds.serveAppend))
		mux.HandleFunc("GET /devices", ds.corsBearerSimple(ds.serveList))
		mux.HandleFunc("OPTIONS /devices/append", ds.corsBearerSimple(func(http.ResponseWriter, *http.Request) {}))
		mux.HandleFunc("OPTIONS /devices", ds.corsBearerSimple(func(http.ResponseWriter, *http.Request) {}))
	}

	if s.idp != nil {
		idp := s.idp
		mux.HandleFunc("GET /oauth/authorize", idp.serveAuthorize)
		mux.HandleFunc("GET /oauth/callback", idp.serveCallback)
		mux.HandleFunc("POST /refresh", idp.corsCredentialed(idp.serveRefresh))
		mux.HandleFunc("OPTIONS /refresh", idp.corsCredentialed(func(http.ResponseWriter, *http.Request) {}))
		mux.HandleFunc("POST /logout", idp.corsCredentialed(idp.serveLogout))
		mux.HandleFunc("OPTIONS /logout", idp.corsCredentialed(func(http.ResponseWriter, *http.Request) {}))
		mux.HandleFunc("POST /device/authorize", idp.serveDeviceAuthorize)
		mux.HandleFunc("GET /device/verify", idp.serveDeviceVerifyGet)
		mux.HandleFunc("POST /device/verify", idp.serveDeviceVerifyPost)
		mux.HandleFunc("POST /device/token", idp.serveDeviceToken)
	}
	return mux
}

// InternalHandler returns direct-TLS routes protected by the supplied principal policy.
func (s *Service) InternalHandler(policy mtls.Policy) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enroll", s.enrollHandler)
	if s.nodeRevocations != nil {
		mux.HandleFunc("GET /node-revocations", s.serveNodeRevocations)
	}
	mux.Handle(authv1connect.NewAuthServiceHandler(s))
	if s.idp != nil {
		mux.HandleFunc("GET /revocations", s.idp.serveRevocations)
	}
	mux.HandleFunc("POST /internal/github/link-status", s.serveGitHubLinkStatus)

	operation := func(r *http.Request) string {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/enroll":
			return operationEnroll
		case r.Method == http.MethodGet && r.URL.Path == "/node-revocations":
			return operationNodeRevocations
		case r.Method == http.MethodPost && r.URL.Path == authv1connect.AuthServiceMintGitHubAccessTokenProcedure:
			return operationCredentialMint
		case r.Method == http.MethodGet && r.URL.Path == "/revocations":
			return operationRevocations
		case r.Method == http.MethodPost && r.URL.Path == "/internal/github/link-status":
			return operationGitHubLink
		default:
			return ""
		}
	}
	return policy.HTTPMiddleware(operation, mux)
}
