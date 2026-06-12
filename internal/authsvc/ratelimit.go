package authsvc

import (
	"net"
	"net/http"
	"sync"

	"golang.org/x/time/rate"
)

// RateLimitConfig sets the per-key token buckets (auth-identity design §3/§6 abuse controls).
// Zero values get the defaults below; env-tunable via cmd/authsvc.
type RateLimitConfig struct {
	// AuthorizePerMin caps per-IP starts of the OAuth flow (/oauth/authorize + /oauth/callback).
	AuthorizePerMin int
	// RefreshPerMin caps /refresh per IP and per account.
	RefreshPerMin int
	// RegistrationPerHour caps new-user creation per IP (inside the callback).
	RegistrationPerHour int
	// DevicePerMin caps device-grant creation, user_code peek/confirm attempts per IP.
	DevicePerMin int
}

func (c RateLimitConfig) withDefaults() RateLimitConfig {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.AuthorizePerMin, 30)
	def(&c.RefreshPerMin, 60)
	def(&c.RegistrationPerHour, 10)
	def(&c.DevicePerMin, 10)
	return c
}

// keyedLimiter is a bounded map of token buckets. Eviction is random-ish (Go map iteration)
// when full — good enough for an abuse-control cache.
type keyedLimiter struct {
	mu    sync.Mutex
	m     map[string]*rate.Limiter
	limit rate.Limit
	burst int
}

const maxLimiterEntries = 16384

func newKeyedLimiter(perInterval int, interval float64) *keyedLimiter {
	return &keyedLimiter{
		m:     map[string]*rate.Limiter{},
		limit: rate.Limit(float64(perInterval) / interval),
		burst: perInterval,
	}
}

func (l *keyedLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.m[key]
	if !ok {
		if len(l.m) >= maxLimiterEntries {
			for k := range l.m {
				delete(l.m, k)
				break
			}
		}
		lim = rate.NewLimiter(l.limit, l.burst)
		l.m[key] = lim
	}
	return lim.Allow()
}

type rateLimiters struct {
	authorize    *keyedLimiter // per IP
	refreshIP    *keyedLimiter // per IP
	refreshAcct  *keyedLimiter // per account
	registration *keyedLimiter // per IP
	device       *keyedLimiter // per IP
}

func newRateLimiters(cfg RateLimitConfig) *rateLimiters {
	cfg = cfg.withDefaults()
	return &rateLimiters{
		authorize:    newKeyedLimiter(cfg.AuthorizePerMin, 60),
		refreshIP:    newKeyedLimiter(cfg.RefreshPerMin, 60),
		refreshAcct:  newKeyedLimiter(cfg.RefreshPerMin, 60),
		registration: newKeyedLimiter(cfg.RegistrationPerHour, 3600),
		device:       newKeyedLimiter(cfg.DevicePerMin, 60),
	}
}

// clientIP extracts the rate-limit key from the connection. The AS terminates TLS itself in
// the MVP deployment; if a trusted proxy fronts it later, swap this for an allowlisted
// X-Forwarded-For parse.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tooMany emits the 429 + Retry-After contract.
func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; retry later")
}
