package web

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dustinfields/i2p-irc/internal/irc"
)

func TestSecurityHeadersMiddleware(t *testing.T) {
	// Setup
	sessions := irc.NewSessionStore()
	tmpl := template.New("test")
	handler := NewHandler(Config{}, sessions, tmpl, nil)

	// Create a test handler wrapped with middleware
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})
	wrappedHandler := handler.SecurityHeadersMiddleware(testHandler)

	// Request
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	wrappedHandler.ServeHTTP(rec, req)

	// Verify headers
	headers := rec.Header()

	expectedHeaders := map[string]string{
		"Content-Security-Policy": "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-src 'self';",
		"X-Frame-Options":         "SAMEORIGIN",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "strict-origin-when-cross-origin",
	}

	for key, expected := range expectedHeaders {
		if got := headers.Get(key); got != expected {
			t.Errorf("Header %s: expected %q, got %q", key, expected, got)
		}
	}
}

func TestCSRFMiddleware(t *testing.T) {
	// Setup
	sessions := irc.NewSessionStore()
	tmpl := template.New("test")
	handler := NewHandler(Config{}, sessions, tmpl, nil)

	// Create a test handler wrapped with middleware
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})
	wrappedHandler := handler.CSRFMiddleware(testHandler)

	// Test 1: GET request should set CSRF cookie and session cookie
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec, req)

	cookies := rec.Result().Cookies()
	var csrfCookie *http.Cookie
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "csrf_token" {
			csrfCookie = c
		}
		if c.Name == "session_id" {
			sessionCookie = c
		}
	}

	if csrfCookie == nil {
		t.Fatal("CSRF cookie not set on GET request")
	}
	// Note: Secure flag is only set for HTTPS requests, not HTTP test requests
	if csrfCookie.Secure {
		t.Error("CSRF cookie should NOT be Secure for HTTP requests")
	}

	// Session cookie should also be set (with Secure flag based on request scheme)
	if sessionCookie == nil {
		t.Fatal("Session cookie not set on GET request")
	}
	if sessionCookie.Secure {
		t.Error("Session cookie should NOT be Secure for HTTP requests")
	}

	token := csrfCookie.Value

	// For subsequent requests, we need to include the session cookie to maintain session context
	// The CSRF token is bound to the session ID, so we need both cookies

	// Test 2: POST request without form token should fail
	req = httptest.NewRequest("POST", "/", nil)
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)

	rec = httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST without token: expected 403, got %d", rec.Code)
	}

	// Test 3: POST request with invalid form token should fail
	req = httptest.NewRequest("POST", "/", strings.NewReader("csrf_token=invalid"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)

	rec = httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST with invalid token: expected 403, got %d", rec.Code)
	}

	// Test 4: POST request with valid form token should succeed
	req = httptest.NewRequest("POST", "/", strings.NewReader("csrf_token="+token))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)

	rec = httptest.NewRecorder()
	wrappedHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("POST with valid token: expected 200, got %d", rec.Code)
	}
}
