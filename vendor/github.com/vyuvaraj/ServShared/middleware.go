package ServShared

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ContextKey is the type for context keys to avoid collisions.
type ContextKey string

const (
	// ClaimsContextKey is the context key for authenticated claims.
	ClaimsContextKey ContextKey = "servverse-claims"
)

// GetClaims extracts claims from request context (set by AuthMiddleware).
func GetClaims(r *http.Request) *Claims {
	if c, ok := r.Context().Value(ClaimsContextKey).(*Claims); ok {
		return c
	}
	return nil
}

// AuthMiddleware returns an HTTP middleware that enforces JWT auth.
//
// Behavior:
//   - If SERV_JWT_SECRET is empty: all requests pass through (dev mode)
//   - If SERV_JWT_SECRET is set: requires valid Bearer JWT on all routes
//   - /healthz and /readyz are always allowed without auth
func AuthMiddleware(next http.Handler) http.Handler {
	secret := os.Getenv("SERV_JWT_SECRET")
	if secret == "" {
		return next // dev mode — no auth enforced
	}

	validator := NewAuthValidator(secret, "", "")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always allow health probes without auth
		path := r.URL.Path
		if path == "/healthz" || path == "/readyz" || path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Extract token
		token, err := ExtractTokenFromHeader(r.Header.Get("Authorization"))
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "Unauthorized: "+err.Error(), "ERR_MISSING_AUTH")
			return
		}

		// Validate
		claims, err := validator.ValidateToken(token)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "Unauthorized: "+err.Error(), "ERR_INVALID_TOKEN")
			return
		}

		// Inject claims into context
		ctx := context.WithValue(r.Context(), ClaimsContextKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GenerateServiceToken creates a long-lived JWT for inter-service communication.
// The token identifies the calling service and has the "service" role.
func GenerateServiceToken(secret string, serviceName string) (string, error) {
	if secret == "" {
		return "", nil // dev mode — no token needed
	}

	claims := Claims{
		Username: serviceName,
		Roles:    []string{"service"},
		Scopes:   []string{"*"},
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "servverse",
			Subject:   serviceName,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(365 * 24 * time.Hour)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// GenerateUserToken creates a JWT for a user with given roles.
func GenerateUserToken(secret string, username string, roles []string, ttl time.Duration) (string, error) {
	claims := Claims{
		Username: username,
		Roles:    roles,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "servverse",
			Subject:   username,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// HasRole checks if the authenticated claims include a specific role.
func HasRole(r *http.Request, role string) bool {
	claims := GetClaims(r)
	if claims == nil {
		return false
	}
	for _, r := range claims.Roles {
		if r == role || r == "admin" {
			return true
		}
	}
	return false
}

func writeAuthError(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{
		"error": msg,
		"code":  code,
	})
}
