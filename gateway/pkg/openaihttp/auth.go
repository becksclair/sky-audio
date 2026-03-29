package openaihttp

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
)

// AuthMiddleware creates a middleware that validates Bearer tokens
// If SKY_AUDIO_API_KEY env var is set, requests must include matching Authorization header
// If unset, all requests are allowed (development mode)
func AuthMiddleware(next http.Handler) http.Handler {
	expectedKey := os.Getenv("SKY_AUDIO_API_KEY")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If no API key configured, skip auth
		if expectedKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Extract Bearer token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeError(w, http.StatusUnauthorized, "invalid_authentication_error", "Authorization header is required")
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			writeError(w, http.StatusUnauthorized, "invalid_authentication_error", "Authorization header must be 'Bearer <token>'")
			return
		}

		token := parts[1]
		// Constant-time comparison to prevent timing attacks
		if subtle.ConstantTimeCompare([]byte(token), []byte(expectedKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid_authentication_error", "Invalid API key")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// OptionalAuthMiddleware is like AuthMiddleware but returns 200 for health checks
// even without auth (for load balancer probes).
// Also supports ?token= query parameter for EventSource/SSE which cannot send custom headers.
func OptionalAuthMiddleware(next http.Handler) http.Handler {
	expectedKey := os.Getenv("SKY_AUDIO_API_KEY")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health check bypass - note: /health is handled at parent mux level
		// This is a safety net for /v1/health if added later
		if r.URL.Path == "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}

		// If no API key configured, skip auth
		if expectedKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Extract token from Authorization header or query parameter
		var token string
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
				writeError(w, http.StatusUnauthorized, "invalid_authentication_error", "Authorization header must be 'Bearer <token>'")
				return
			}
			token = parts[1]
		} else {
			// Fallback to query parameter for EventSource/SSE support
			token = r.URL.Query().Get("token")
		}

		if token == "" {
			writeError(w, http.StatusUnauthorized, "invalid_authentication_error", "Authorization header or token query parameter is required")
			return
		}

		// Constant-time comparison to prevent timing attacks
		if subtle.ConstantTimeCompare([]byte(token), []byte(expectedKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid_authentication_error", "Invalid API key")
			return
		}

		next.ServeHTTP(w, r)
	})
}
