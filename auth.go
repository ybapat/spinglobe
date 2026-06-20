package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// gatewayClaims are the JWT claims the gateway cares about.
type gatewayClaims struct {
	Tier string `json:"tier"`
	jwt.RegisteredClaims
}

// tierName constants — normalised to lowercase.
const (
	tierFree       = "free"
	tierPremium    = "premium"
	tierEnterprise = "enterprise"
)

// TierFor returns the TierLimits for the named tier, defaulting to Free.
func TierFor(tier string, cfg Config) TierLimits {
	switch strings.ToLower(tier) {
	case tierPremium:
		return cfg.TierPremium
	case tierEnterprise:
		return cfg.TierEnterprise
	default:
		return cfg.TierFree
	}
}

// JWTAuthMiddleware validates the Bearer JWT in Authorization, injects tier
// and subject into the request context, and rejects invalid or expired tokens.
func JWTAuthMiddleware(secret string, logger *zap.Logger) func(http.Handler) http.Handler {
	keyFunc := func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				logger.Debug("missing or malformed Authorization header",
					zap.String("path", r.URL.Path),
					zap.String("remote_addr", r.RemoteAddr),
				)
				http.Error(w, "authorization required", http.StatusUnauthorized)
				return
			}

			claims := &gatewayClaims{}
			token, err := jwt.ParseWithClaims(raw, claims, keyFunc,
				jwt.WithExpirationRequired(),
				jwt.WithIssuedAt(),
			)
			if err != nil || !token.Valid {
				status, msg := classifyJWTError(err)
				logger.Warn("JWT validation failed",
					zap.String("path", r.URL.Path),
					zap.Int("status", status),
					zap.Error(err),
				)
				http.Error(w, msg, status)
				return
			}

			tier := strings.ToLower(claims.Tier)
			if tier == "" {
				tier = tierFree
			}

			r = withValue(r, ctxKeySubject, claims.Subject)
			r = withValue(r, ctxKeyTier, tier)

			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the raw JWT string from "Authorization: Bearer <token>".
func bearerToken(r *http.Request) (string, bool) {
	hdr := r.Header.Get("Authorization")
	if hdr == "" {
		return "", false
	}
	parts := strings.SplitN(hdr, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}

// classifyJWTError maps jwt errors to HTTP status codes and messages.
func classifyJWTError(err error) (int, string) {
	if err == nil {
		return http.StatusUnauthorized, "invalid token"
	}
	if errors.Is(err, jwt.ErrTokenExpired) {
		return http.StatusForbidden, "token expired"
	}
	return http.StatusUnauthorized, "invalid token"
}
