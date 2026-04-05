package openaihttp

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
)

func tokenMatches(token string, expectedKey string, extraValidator func(string) bool) bool {
	if expectedKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(expectedKey)) == 1 {
		return true
	}
	return extraValidator != nil && extraValidator(token)
}

func extractBearerToken(authHeader string) (string, bool) {
	if authHeader == "" {
		return "", false
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", false
	}
	return parts[1], true
}

func extractWebSocketSubprotocolToken(header string) string {
	for _, raw := range strings.Split(header, ",") {
		protocol := strings.TrimSpace(raw)
		if token, ok := strings.CutPrefix(protocol, "openai-insecure-api-key."); ok && token != "" {
			return token
		}
	}
	return ""
}

func extractRequestToken(r *http.Request) (string, string) {
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		token, ok := extractBearerToken(authHeader)
		if !ok {
			return "", "Authorization header must be 'Bearer <token>'"
		}
		return token, ""
	}
	if token := extractWebSocketSubprotocolToken(r.Header.Get("Sec-WebSocket-Protocol")); token != "" {
		return token, ""
	}
	if token := r.URL.Query().Get("token"); token != "" {
		return token, ""
	}
	return "", "Authorization header, websocket subprotocol token, or token query parameter is required"
}

// AuthMiddleware creates a middleware that validates Bearer tokens
// If SKY_AUDIO_API_KEY env var is set, requests must include matching Authorization header
// If unset, all requests are allowed (development mode)
func AuthMiddleware(next http.Handler) http.Handler {
	expectedKey := os.Getenv("SKY_AUDIO_API_KEY")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expectedKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		token, msg := extractRequestToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "invalid_authentication_error", msg)
			return
		}
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
	return OptionalAuthMiddlewareWithValidator(next, nil)
}

func OptionalAuthMiddlewareWithValidator(next http.Handler, extraValidator func(string) bool) http.Handler {
	expectedKey := os.Getenv("SKY_AUDIO_API_KEY")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		if expectedKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		token, msg := extractRequestToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "invalid_authentication_error", msg)
			return
		}
		if !tokenMatches(token, expectedKey, extraValidator) {
			writeError(w, http.StatusUnauthorized, "invalid_authentication_error", "Invalid API key")
			return
		}

		next.ServeHTTP(w, r)
	})
}
