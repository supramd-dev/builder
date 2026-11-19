package performance

import "net/http"

// AuthWrapper wraps a handler so that only a signed-in user reaches it. The
// host hands over its own — *api.Server's AuthMiddleware method is one — which
// is how this package gets authentication without importing the API package:
// the two stay independent, and the module can be tested with a stub.
type AuthWrapper func(next http.HandlerFunc) http.HandlerFunc

// Service is the module. A host server builds one and registers it; nothing
// else in the codebase needs to know it exists.
type Service struct {
	// Store is where the data comes from: *store.Store in production, a fake
	// in the module's own tests.
	Store Loader
}

// New returns the module backed by the given loader.
func New(l Loader) *Service { return &Service{Store: l} }

// Register mounts the module's routes on mux. auth is the host's "signed-in
// users only" wrapper; it is required — a nil one is a programming error at
// startup rather than a route that quietly serves every caller.
func (s *Service) Register(mux *http.ServeMux, auth AuthWrapper) {
	if auth == nil {
		panic("performance: Register needs the host's auth wrapper")
	}
	mux.HandleFunc("/api/performance", auth(s.handleHistory))
}
