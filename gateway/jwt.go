package main

// ---------------------------------------------------------------------------
// TCA JWT — stdlib only, zero external dependencies.
//
// Implements HS256 JWT sign and verify using crypto/hmac, crypto/sha256,
// and encoding/base64. No reflection. No interface{} gymnastics.
//
// Design decisions:
//   - HS256 only. SETI uses a shared secret, not asymmetric keys.
//   - Claims are a flat map[string]interface{} at the wire level.
//     Callers extract typed fields after verification.
//   - Expiry is validated on every Verify call. Issued-at is not checked
//     (clock skew between issuer and gateway is not a threat model concern).
//   - No support for algorithm confusion — the header alg field is always
//     written as "HS256" on sign and always checked as "HS256" on verify.
// ---------------------------------------------------------------------------

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// jwtHeader is the fixed base64url-encoded header for all tokens.
// {"alg":"HS256","typ":"JWT"}
var jwtHeader = base64url(mustMarshal(map[string]string{
	"alg": "HS256",
	"typ": "JWT",
}))

// JWTClaims holds the fields gateway extracts from every token.
// Both signal-clearance and policy tokens carry these fields.
type JWTClaims struct {
	WranglerID     string
	ClearanceLevel string
}

// SignJWT signs a claims map with the given secret and returns a JWT string.
func SignJWT(claims map[string]interface{}, secret []byte) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("jwt: marshal claims: %w", err)
	}

	body := jwtHeader + "." + base64url(payload)
	sig := hmacSHA256([]byte(body), secret)
	return body + "." + base64url(sig), nil
}

// VerifyJWT validates signature and expiry, returns extracted claims.
func VerifyJWT(tokenStr string, secret []byte) (*JWTClaims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("jwt: malformed token")
	}

	// Verify header
	headerJSON, err := base64urlDecode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("jwt: decode header: %w", err)
	}
	var header map[string]string
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("jwt: parse header: %w", err)
	}
	if header["alg"] != "HS256" {
		return nil, fmt.Errorf("jwt: unexpected algorithm %q", header["alg"])
	}

	// Verify signature
	body := parts[0] + "." + parts[1]
	expected := base64url(hmacSHA256([]byte(body), secret))
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return nil, fmt.Errorf("jwt: invalid signature")
	}

	// Decode claims
	claimsJSON, err := base64urlDecode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("jwt: decode claims: %w", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(claimsJSON, &raw); err != nil {
		return nil, fmt.Errorf("jwt: parse claims: %w", err)
	}

	// Validate expiry
	exp, ok := raw["exp"].(float64)
	if !ok {
		return nil, fmt.Errorf("jwt: missing exp claim")
	}
	if time.Now().Unix() > int64(exp) {
		return nil, fmt.Errorf("jwt: token expired")
	}

	// Extract fields gateway needs
	claims := &JWTClaims{}
	if v, ok := raw["wrangler_id"].(string); ok {
		claims.WranglerID = v
	}
	if v, ok := raw["clearance_level"].(string); ok {
		claims.ClearanceLevel = v
	}

	return claims, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func hmacSHA256(data, secret []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write(data)
	return h.Sum(nil)
}

func base64url(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func base64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
