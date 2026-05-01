// Package auth provides JWT token generation, validation, and revocation for
// the Control Plane. Tokens are signed with HMAC-SHA256 and include a unique
// jti claim that can be blacklisted in Redis (system-design.md §5.3.1).
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims extends the standard RegisteredClaims with Control Plane fields.
type Claims struct {
	jwt.RegisteredClaims
	OrgID     string `json:"org_id"`
	Role      string `json:"role"`
	Username  string `json:"username"`
	TokenType string `json:"token_type"` // "access" or "refresh"
}

// RedisClient is the subset of Redis operations required by the JWT service.
type RedisClient interface {
	Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error
	Exists(ctx context.Context, keys ...string) (int64, error)
}

// Service defines the JWT operations used throughout the Control Plane.
type Service interface {
	// GenerateAccessToken creates a signed access token for the given subject.
	GenerateAccessToken(subject, orgID, role, username string, ttl time.Duration) (string, error)
	// GenerateRefreshToken creates a signed refresh token for the given subject.
	GenerateRefreshToken(subject, orgID, role, username string, ttl time.Duration) (string, error)
	// ValidateToken parses and validates a signed JWT string, returning its claims.
	ValidateToken(tokenStr string) (*Claims, error)
	// RevokeToken adds the token's jti to the Redis blacklist with a TTL equal
	// to the token's remaining lifetime.
	RevokeToken(ctx context.Context, tokenStr string) error
	// IsRevoked reports whether the given jti has been blacklisted.
	IsRevoked(ctx context.Context, jti string) (bool, error)
}

type jwtService struct {
	secret []byte
	redis  RedisClient
}

// New creates a JWT service. redis may be nil, in which case IsRevoked always
// returns false and RevokeToken is a no-op.
func New(secret string, redis RedisClient) Service {
	return &jwtService{
		secret: []byte(secret),
		redis:  redis,
	}
}

// GenerateAccessToken creates a signed access token.
func (s *jwtService) GenerateAccessToken(subject, orgID, role, username string, ttl time.Duration) (string, error) {
	return s.generate(subject, orgID, role, username, "access", ttl)
}

// GenerateRefreshToken creates a signed refresh token.
func (s *jwtService) GenerateRefreshToken(subject, orgID, role, username string, ttl time.Duration) (string, error) {
	return s.generate(subject, orgID, role, username, "refresh", ttl)
}

func (s *jwtService) generate(subject, orgID, role, username, tokenType string, ttl time.Duration) (string, error) {
	now := time.Now().UTC()
	jti := uuid.New().String()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        jti,
		},
		OrgID:     orgID,
		Role:      role,
		Username:  username,
		TokenType: tokenType,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.secret)
}

// ValidateToken parses and validates a signed JWT string.
func (s *jwtService) ValidateToken(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	if !token.Valid {
		return nil, errors.New("token is not valid")
	}
	return claims, nil
}

// RevokeToken blacklists the token's jti in Redis.
func (s *jwtService) RevokeToken(ctx context.Context, tokenStr string) error {
	if s.redis == nil {
		return nil
	}
	claims, err := s.ValidateToken(tokenStr)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	jti := claims.ID
	if jti == "" {
		return errors.New("revoke token: token has no jti")
	}
	remaining := time.Until(claims.ExpiresAt.Time)
	if remaining <= 0 {
		// Already expired — no need to blacklist.
		return nil
	}
	return s.redis.Set(ctx, cache.JWTBlacklistKey(jti), "1", remaining)
}

// IsRevoked checks whether the given jti has been blacklisted in Redis.
func (s *jwtService) IsRevoked(ctx context.Context, jti string) (bool, error) {
	if s.redis == nil {
		return false, nil
	}
	n, err := s.redis.Exists(ctx, cache.JWTBlacklistKey(jti))
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
