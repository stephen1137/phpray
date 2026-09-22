package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ─── JWT Claims ─────────────────────────────────────────────────────────────

type JWTClaims struct {
	Sub     string   `json:"sub"`
	Role    string   `json:"role"`
	Domains []string `json:"domains,omitempty"`
	Exp     int64    `json:"exp"`
	Iat     int64    `json:"iat"`
}

type contextKey int

const claimsKey contextKey = 1

// ClaimsFromContext extracts JWT claims from request context.
func ClaimsFromContext(ctx context.Context) *JWTClaims {
	c, _ := ctx.Value(claimsKey).(*JWTClaims)
	return c
}

// ─── Minimal JWT: encode / decode / verify (HS256 only) ─────────────────────

func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func signHS256(signingInput, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return base64URLEncode(mac.Sum(nil))
}

// GenerateToken creates a signed JWT string.
func GenerateToken(secret string, claims JWTClaims) (string, error) {
	if claims.Iat == 0 {
		claims.Iat = time.Now().Unix()
	}

	header := base64URLEncode([]byte(`{"alg":"HS256","typ":"JWT"}`))

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	payloadB64 := base64URLEncode(payload)

	signingInput := header + "." + payloadB64
	sig := signHS256(signingInput, secret)

	return signingInput + "." + sig, nil
}

// VerifyToken parses a JWT string, checks signature and expiration.
func VerifyToken(secret, tokenStr string) (*JWTClaims, error) {
	parts := strings.SplitN(tokenStr, ".", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}

	signingInput := parts[0] + "." + parts[1]
	expectedSig := signHS256(signingInput, secret)

	if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
		return nil, fmt.Errorf("invalid signature")
	}

	payloadJSON, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid payload encoding")
	}

	var claims JWTClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, fmt.Errorf("invalid payload JSON")
	}

	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, fmt.Errorf("token expired")
	}

	return &claims, nil
}

// ─── HTTP Middleware ─────────────────────────────────────────────────────────

// AuthMiddleware returns a chi-compatible middleware that validates JWT.
// If secret is empty, all requests pass through (auth disabled).
func AuthMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				next.ServeHTTP(w, r)
				return
			}

			tokenStr := extractToken(r)
			if tokenStr == "" {
				writeError(w, 401, "missing authentication token")
				return
			}

			claims, err := VerifyToken(secret, tokenStr)
			if err != nil {
				writeError(w, 401, "invalid token: "+err.Error())
				return
			}

			ctx := context.WithValue(r.Context(), claimsKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// WSAuthMiddleware validates JWT from query param for WebSocket upgrades.
func WSAuthMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				next.ServeHTTP(w, r)
				return
			}

			tokenStr := r.URL.Query().Get("token")
			if tokenStr == "" {
				// Also check Authorization header as fallback
				tokenStr = extractBearerToken(r)
			}
			if tokenStr == "" {
				writeError(w, 401, "missing authentication token")
				return
			}

			claims, err := VerifyToken(secret, tokenStr)
			if err != nil {
				writeError(w, 401, "invalid token: "+err.Error())
				return
			}

			ctx := context.WithValue(r.Context(), claimsKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractToken(r *http.Request) string {
	// 1. Authorization: Bearer <token>
	if t := extractBearerToken(r); t != "" {
		return t
	}
	// 2. ?token=<token> query param
	return r.URL.Query().Get("token")
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return auth[7:]
	}
	return ""
}

// ─── Domain Filtering ───────────────────────────────────────────────────────

// AllowedDomains returns the domain list from JWT claims for "user" role.
// For "admin" role (or no claims), returns nil meaning "all domains".
func AllowedDomains(r *http.Request) []string {
	claims := ClaimsFromContext(r.Context())
	if claims == nil || claims.Role == "admin" {
		return nil
	}
	return claims.Domains
}

// DomainFilter returns a SQL WHERE clause fragment and args to restrict
// queries by the user's allowed domains. If no restriction, returns empty.
func DomainFilter(r *http.Request, column string) (string, []interface{}) {
	domains := AllowedDomains(r)
	if len(domains) == 0 {
		return "", nil
	}

	placeholders := make([]string, len(domains))
	args := make([]interface{}, len(domains))
	for i, d := range domains {
		placeholders[i] = column + " LIKE ?"
		args[i] = "%" + d + "%"
	}
	return " AND (" + strings.Join(placeholders, " OR ") + ")", args
}

// ─── Token CLI ──────────────────────────────────────────────────────────────

func cmdToken(args []string) {
	var (
		secret  string
		sub     string
		role    string
		domains string
		expStr  string
	)

	// Simple flag parsing (no flag.FlagSet to keep it minimal)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-secret":
			i++
			if i < len(args) {
				secret = args[i]
			}
		case "-sub":
			i++
			if i < len(args) {
				sub = args[i]
			}
		case "-role":
			i++
			if i < len(args) {
				role = args[i]
			}
		case "-domains":
			i++
			if i < len(args) {
				domains = args[i]
			}
		case "-exp":
			i++
			if i < len(args) {
				expStr = args[i]
			}
		default:
			fmt.Fprintf(os.Stderr, "Unknown flag: %s\n", args[i])
			fmt.Fprintln(os.Stderr, "Usage: phpray-collector token -secret <key> -sub <user> -role <admin|user> [-domains \"a.com,b.com\"] [-exp 720h]")
			os.Exit(1)
		}
	}

	if secret == "" {
		secret = os.Getenv("PHPRAY_JWT_SECRET")
	}
	if secret == "" {
		fmt.Fprintln(os.Stderr, "Error: -secret flag or PHPRAY_JWT_SECRET env required")
		os.Exit(1)
	}
	if sub == "" || role == "" {
		fmt.Fprintln(os.Stderr, "Error: -sub and -role flags are required")
		fmt.Fprintln(os.Stderr, "Usage: phpray-collector token -secret <key> -sub <user> -role <admin|user> [-domains \"a.com,b.com\"] [-exp 720h]")
		os.Exit(1)
	}
	if role != "admin" && role != "user" {
		fmt.Fprintln(os.Stderr, "Error: -role must be 'admin' or 'user'")
		os.Exit(1)
	}

	claims := JWTClaims{
		Sub:  sub,
		Role: role,
		Iat:  time.Now().Unix(),
	}

	// Parse domains
	if domains != "" {
		for _, d := range strings.Split(domains, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				claims.Domains = append(claims.Domains, d)
			}
		}
	}

	// Parse expiration duration
	if expStr != "" {
		dur, err := time.ParseDuration(expStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid -exp duration: %v\n", err)
			os.Exit(1)
		}
		claims.Exp = time.Now().Add(dur).Unix()
	} else {
		// Default: 30 days
		claims.Exp = time.Now().Add(30 * 24 * time.Hour).Unix()
	}

	token, err := GenerateToken(secret, claims)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(token)
}
