// Package token issues and verifies the access-token JWTs and the opaque
// refresh tokens. Claims match the Python service (sub/dev/iat/exp/iss,
// HS256) so existing iOS token handling works unchanged.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"livetranslate/server/internal/config"
)

var ErrInvalid = errors.New("invalid access token")

type Manager struct {
	secret     []byte
	issuer     string
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func NewManager(cfg *config.Config) *Manager {
	return &Manager{
		secret:     []byte(cfg.JWTSecret),
		issuer:     cfg.JWTIssuer,
		accessTTL:  cfg.AccessTokenTTL,
		refreshTTL: cfg.RefreshTokenTTL,
	}
}

// AccessClaims is what a valid access JWT carries. sid is the token id
// (jti); role is advisory only — authorization always re-checks the DB.
type AccessClaims struct {
	UserID   string
	DeviceID string
	TokenID  string
	Role     string
}

func (m *Manager) NewAccessToken(userID, deviceID, role string) (string, time.Duration, error) {
	now := time.Now()
	ttl := m.accessTTL
	claims := jwt.MapClaims{
		"iss": m.issuer,
		"sub": userID,
		"dev": deviceID,
		"jti": config.RandomHex(16),
		"rol": role,
		"iat": now.Unix(),
		"exp": now.Add(ttl).Unix(),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	return signed, ttl, err
}

func (m *Manager) VerifyAccessToken(raw string) (*AccessClaims, error) {
	token, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return m.secret, nil
	}, jwt.WithIssuer(m.issuer), jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid {
		return nil, ErrInvalid
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, ErrInvalid
	}
	sub, _ := claims["sub"].(string)
	dev, _ := claims["dev"].(string)
	jti, _ := claims["jti"].(string)
	rol, _ := claims["rol"].(string)
	if sub == "" || dev == "" {
		return nil, ErrInvalid
	}
	return &AccessClaims{UserID: sub, DeviceID: dev, TokenID: jti, Role: rol}, nil
}

// --- Refresh tokens ---------------------------------------------------------
// Opaque high-entropy values; only the SHA-256 hash is stored server-side.

// NewRefreshToken returns (plain, hash, ttl). The plain value goes to the
// client exactly once; the hash is what the DB keeps.
func (m *Manager) NewRefreshToken() (plain, hash string, ttl time.Duration) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	plain = base64URL(b)
	return plain, HashToken(plain), m.refreshTTL
}

// HashToken is the DB-side transform. Same as Python's sha256 of the token.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

func base64URL(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	// 48 bytes -> 64 chars of 6 bits each.
	var out []byte
	acc, bits := 0, 0
	for _, v := range b {
		acc = (acc << 8) | int(v)
		bits += 8
		for bits >= 6 {
			bits -= 6
			out = append(out, alphabet[(acc>>bits)&0x3f])
		}
	}
	return string(out)
}

// NewOpaqueToken generates a generic URL-safe random token (email codes,
// reset tokens, admin session tokens). 32 bytes entropy.
func NewOpaqueToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NewEmailCode generates a 6-digit numeric verification code.
func NewEmailCode() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return fmt.Sprintf("%06d", n%1000000)
}
