package web

import (
	"net/http"
	"sync"
	"time"
)

// RateLimiter provides simple per-session rate limiting
type RateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	limit    int           // max requests
	window   time.Duration // time window
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
	// Start cleanup goroutine
	go rl.cleanup()
	return rl
}

// Allow checks if a request should be allowed for the given key (session ID)
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	windowStart := now.Add(-rl.window)

	// Filter out old requests
	var recent []time.Time
	for _, t := range rl.requests[key] {
		if t.After(windowStart) {
			recent = append(recent, t)
		}
	}

	// Check if under limit
	if len(recent) >= rl.limit {
		rl.requests[key] = recent
		return false
	}

	// Add current request
	rl.requests[key] = append(recent, now)
	return true
}

// cleanup periodically removes old entries
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		windowStart := now.Add(-rl.window)

		for key, times := range rl.requests {
			var recent []time.Time
			for _, t := range times {
				if t.After(windowStart) {
					recent = append(recent, t)
				}
			}
			if len(recent) == 0 {
				delete(rl.requests, key)
			} else {
				rl.requests[key] = recent
			}
		}
		rl.mu.Unlock()
	}
}

// RateLimitMiddleware creates middleware that rate limits by session cookie
func (h *Handler) RateLimitMiddleware(limiter *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Get session ID from cookie (don't create one just for rate limiting)
			var key string
			cookie, err := r.Cookie(SessionCookieName)
			if err == nil && cookie.Value != "" {
				key = cookie.Value
			} else {
				// Fall back to remote address for unauthenticated requests
				key = r.RemoteAddr
			}

			if !limiter.Allow(key) {
				http.Error(w, "Too many requests. Please slow down.", http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
