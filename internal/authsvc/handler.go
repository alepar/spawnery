package authsvc

import "net/http"

// Handler returns the AS's HTTP surface. The skeleton serves liveness and the Root CA for
// distribution; enrollment (sp-0qc) and session signing (sp-3ca) add their routes here. NOTE: the Root
// CA is the trust anchor and is meant to be pinned OUT-OF-BAND (baked into client bundles); this
// endpoint exists for bootstrap/ops convenience, not as the trust mechanism.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /ca/root", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		_, _ = w.Write(s.RootCAPEM())
	})
	return mux
}
