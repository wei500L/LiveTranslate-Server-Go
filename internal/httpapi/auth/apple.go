package authapi

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"livetranslate/server/internal/config"
)

// AppleTokenError marks verification failures with client-safe messages.
type AppleTokenError struct{ Msg string }

func (e *AppleTokenError) Error() string { return e.Msg }

// jwksCache keeps Apple's public keys for an hour, refreshing on miss.
type jwksCache struct {
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

var appleKeys = &jwksCache{keys: map[string]*rsa.PublicKey{}}

type jwksResponse struct {
	Keys []struct {
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func (c *jwksCache) get(ctx context.Context, jwksURL, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.fetched) < time.Hour {
		if k, ok := c.keys[kid]; ok {
			return k, nil
		}
	}
	if err := c.fetch(ctx, jwksURL); err != nil {
		return nil, err
	}
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, &AppleTokenError{"no matching JWKS key"}
}

func (c *jwksCache) fetch(ctx context.Context, jwksURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &AppleTokenError{"jwks unavailable"}
	}
	var body jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("jwks decode: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range body.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err := decodeB64Int(k.N)
		if err != nil {
			continue
		}
		e, err := decodeB64Int(k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: n, E: int(e.Int64())}
	}
	c.keys = keys
	c.fetched = time.Now()
	return nil
}

func decodeB64Int(s string) (*big.Int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(raw), nil
}

// VerifyAppleIdentity validates the Sign in with Apple identity token:
// RS256 against Apple's JWKS, issuer, audience == bundle id, expiry, and a
// basic iat sanity check. Returns the verified `sub` — the client-supplied
// sub is never trusted.
func VerifyAppleIdentity(ctx context.Context, cfg *config.Config, token string) (string, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, &AppleTokenError{"unexpected algorithm"}
		}
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, &AppleTokenError{"token header has no kid"}
		}
		return appleKeys.get(ctx, cfg.AppleJWKSURL, kid)
	}, jwt.WithAudience(cfg.AppleBundleID), jwt.WithIssuer("https://appleid.apple.com"),
		jwt.WithValidMethods([]string{"RS256"}))
	if err != nil {
		var ate *AppleTokenError
		if errors.As(err, &ate) {
			return "", ate
		}
		return "", &AppleTokenError{"token failed verification"}
	}
	if !parsed.Valid {
		return "", &AppleTokenError{"token failed verification"}
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return "", &AppleTokenError{"token failed verification"}
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", &AppleTokenError{"token has no subject"}
	}
	if iat, ok := claims["iat"].(float64); ok && iat > float64(time.Now().Add(10*time.Minute).Unix()) {
		return "", &AppleTokenError{"token issued in the future"}
	}
	return sub, nil
}
