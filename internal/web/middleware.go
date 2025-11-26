package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// SecurityHeadersMiddleware adds security headers to responses
func (h *Handler) SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content Security Policy - allow embedding in I2P website iframes
		// Note: script-src 'none' since this is a JavaScript-free application
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'none'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-src 'self'; frame-ancestors 'self' https://beta.i2p.net https://i2p.net https://geti2p.net")

		// Prevent MIME type sniffing
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Referrer Policy
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// Add X-Frame-Options for older browsers
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")

		next.ServeHTTP(w, r)
	})
}

// generateCSRFToken creates a CSRF token bound to the session ID using HMAC
func generateCSRFToken(sessionID string) (string, error) {
	// Generate random bytes for the token
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", err
	}
	random := hex.EncodeToString(randomBytes)

	// Create HMAC signature binding token to session
	mac := hmac.New(sha256.New, []byte(sessionID))
	mac.Write([]byte(random))
	signature := hex.EncodeToString(mac.Sum(nil))

	// Token format: random.signature
	return random + "." + signature, nil
}

// verifyCSRFToken verifies a CSRF token is valid for the given session ID
func verifyCSRFToken(token, sessionID string) bool {
	if token == "" || sessionID == "" {
		return false
	}

	// Split token into random and signature parts
	parts := splitToken(token)
	if len(parts) != 2 {
		return false
	}
	random, providedSig := parts[0], parts[1]

	// Recompute expected signature
	mac := hmac.New(sha256.New, []byte(sessionID))
	mac.Write([]byte(random))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	// Constant-time comparison
	return hmac.Equal([]byte(providedSig), []byte(expectedSig))
}

// splitToken splits a token on the first period
func splitToken(token string) []string {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			return []string{token[:i], token[i+1:]}
		}
	}
	return []string{token}
}

// CSRFMiddleware adds CSRF protection with session-bound tokens
func (h *Handler) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get or create session ID
		sessionID := h.getOrCreateSessionID(w, r)

		// Get existing CSRF token from cookie
		csrfCookie, err := r.Cookie("csrf_token")
		var token string

		// Check if we need a new token
		needNewToken := err != nil || csrfCookie.Value == "" || !verifyCSRFToken(csrfCookie.Value, sessionID)

		if needNewToken {
			// Generate new session-bound token
			token, err = generateCSRFToken(sessionID)
			if err != nil {
				// Cryptographic failure - return 500 rather than use weak randomness
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}

			http.SetCookie(w, &http.Cookie{
				Name:     "csrf_token",
				Value:    token,
				Path:     "/",
				HttpOnly: false, // Must be readable by forms
				Secure:   true,
				SameSite: http.SameSiteStrictMode,
			})
		} else {
			token = csrfCookie.Value
		}

		// For POST requests, verify the token
		if r.Method == http.MethodPost {
			submittedToken := r.FormValue("csrf_token")
			if !verifyCSRFToken(submittedToken, sessionID) {
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
