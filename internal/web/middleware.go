package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// SecurityHeadersMiddleware adds security headers to responses
func (h *Handler) SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content Security Policy
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-src 'self';")

		// Prevent clickjacking
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")

		// Prevent MIME type sniffing
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Referrer Policy
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		next.ServeHTTP(w, r)
	})
}

// CSRFMiddleware adds CSRF protection
func (h *Handler) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ensure we have a session ID (cookie) set
		h.getOrCreateSessionID(w, r)

		// Let's try to get the CSRF token from a cookie
		csrfCookie, err := r.Cookie("csrf_token")
		var token string
		if err != nil || csrfCookie.Value == "" {
			// Generate new token
			bytes := make([]byte, 32)
			rand.Read(bytes)
			token = hex.EncodeToString(bytes)

			http.SetCookie(w, &http.Cookie{
				Name:     "csrf_token",
				Value:    token,
				Path:     "/",
				HttpOnly: false,
				Secure:   true,
				SameSite: http.SameSiteStrictMode,
			})
		} else {
			token = csrfCookie.Value
		}

		// For POST requests, verify the token
		if r.Method == http.MethodPost {
			submittedToken := r.FormValue("csrf_token")
			if submittedToken == "" || submittedToken != token {
				http.Error(w, "Invalid CSRF token", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// GetCSRFToken retrieves the CSRF token for the current request
func (h *Handler) GetCSRFToken(r *http.Request) string {
	cookie, err := r.Cookie("csrf_token")
	if err != nil {
		return ""
	}
	return cookie.Value
}
