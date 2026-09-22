package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testSecret = "test-secret-key-123"

func TestGenerateAndVerifyToken(t *testing.T) {
	claims := JWTClaims{
		Sub:  "admin",
		Role: "admin",
		Exp:  time.Now().Add(time.Hour).Unix(),
		Iat:  time.Now().Unix(),
	}

	token, err := GenerateToken(testSecret, claims)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if token == "" {
		t.Fatal("GenerateToken returned empty string")
	}

	verified, err := VerifyToken(testSecret, token)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if verified.Sub != "admin" {
		t.Errorf("Sub = %q, want %q", verified.Sub, "admin")
	}
	if verified.Role != "admin" {
		t.Errorf("Role = %q, want %q", verified.Role, "admin")
	}
}

func TestVerifyToken_WrongSecret(t *testing.T) {
	claims := JWTClaims{
		Sub:  "user1",
		Role: "user",
		Exp:  time.Now().Add(time.Hour).Unix(),
	}

	token, err := GenerateToken(testSecret, claims)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	_, err = VerifyToken("wrong-secret", token)
	if err == nil {
		t.Fatal("expected error for wrong secret, got nil")
	}
}

func TestVerifyToken_Expired(t *testing.T) {
	claims := JWTClaims{
		Sub:  "user1",
		Role: "user",
		Exp:  time.Now().Add(-time.Hour).Unix(),
	}

	token, err := GenerateToken(testSecret, claims)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	_, err = VerifyToken(testSecret, token)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func TestVerifyToken_InvalidFormat(t *testing.T) {
	_, err := VerifyToken(testSecret, "not.a.valid.jwt.token")
	if err == nil {
		t.Fatal("expected error for invalid format")
	}

	_, err = VerifyToken(testSecret, "just-garbage")
	if err == nil {
		t.Fatal("expected error for garbage token")
	}
}

func TestVerifyToken_UserWithDomains(t *testing.T) {
	claims := JWTClaims{
		Sub:     "user1",
		Role:    "user",
		Domains: []string{"example.com", "shop.net"},
		Exp:     time.Now().Add(time.Hour).Unix(),
	}

	token, err := GenerateToken(testSecret, claims)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	verified, err := VerifyToken(testSecret, token)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if verified.Role != "user" {
		t.Errorf("Role = %q, want %q", verified.Role, "user")
	}
	if len(verified.Domains) != 2 {
		t.Fatalf("Domains len = %d, want 2", len(verified.Domains))
	}
	if verified.Domains[0] != "example.com" {
		t.Errorf("Domains[0] = %q, want %q", verified.Domains[0], "example.com")
	}
}

func TestAuthMiddleware_Disabled(t *testing.T) {
	handler := AuthMiddleware("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/api/v1/traces", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("auth disabled: status = %d, want 200", rec.Code)
	}
}

func TestAuthMiddleware_NoToken(t *testing.T) {
	handler := AuthMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/api/v1/traces", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddleware_ValidBearer(t *testing.T) {
	claims := JWTClaims{
		Sub:  "admin",
		Role: "admin",
		Exp:  time.Now().Add(time.Hour).Unix(),
	}
	token, _ := GenerateToken(testSecret, claims)

	var gotClaims *JWTClaims
	handler := AuthMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClaims = ClaimsFromContext(r.Context())
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/api/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("valid bearer: status = %d, want 200", rec.Code)
	}
	if gotClaims == nil {
		t.Fatal("claims not set in context")
	}
	if gotClaims.Sub != "admin" {
		t.Errorf("claims.Sub = %q, want %q", gotClaims.Sub, "admin")
	}
}

func TestAuthMiddleware_QueryParam(t *testing.T) {
	claims := JWTClaims{
		Sub:  "ws-user",
		Role: "user",
		Exp:  time.Now().Add(time.Hour).Unix(),
	}
	token, _ := GenerateToken(testSecret, claims)

	handler := AuthMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/api/v1/traces?token="+token, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("query param: status = %d, want 200", rec.Code)
	}
}

func TestDomainFilter_Admin(t *testing.T) {
	claims := &JWTClaims{Sub: "admin", Role: "admin", Exp: time.Now().Add(time.Hour).Unix()}

	req := httptest.NewRequest("GET", "/", nil)
	ctx := context.WithValue(req.Context(), claimsKey, claims)
	req = req.WithContext(ctx)

	clause, args := DomainFilter(req, "host")
	if clause != "" {
		t.Errorf("admin should have empty domain filter, got %q", clause)
	}
	if args != nil {
		t.Errorf("admin should have nil args, got %v", args)
	}
}

func TestDomainFilter_User(t *testing.T) {
	claims := &JWTClaims{
		Sub:     "user1",
		Role:    "user",
		Domains: []string{"example.com", "shop.net"},
		Exp:     time.Now().Add(time.Hour).Unix(),
	}

	req := httptest.NewRequest("GET", "/", nil)
	ctx := context.WithValue(req.Context(), claimsKey, claims)
	req = req.WithContext(ctx)

	clause, args := DomainFilter(req, "host")
	if clause == "" {
		t.Fatal("user should have domain filter clause")
	}
	if len(args) != 2 {
		t.Fatalf("expected 2 args, got %d", len(args))
	}
	if args[0] != "%example.com%" {
		t.Errorf("args[0] = %v, want %%example.com%%", args[0])
	}
}

func TestDomainFilter_NoClaims(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	clause, args := DomainFilter(req, "host")
	if clause != "" {
		t.Errorf("no claims should have empty domain filter, got %q", clause)
	}
	if args != nil {
		t.Errorf("no claims should have nil args, got %v", args)
	}
}

func TestWSAuthMiddleware_QueryParam(t *testing.T) {
	claims := JWTClaims{Sub: "ws", Role: "admin", Exp: time.Now().Add(time.Hour).Unix()}
	token, _ := GenerateToken(testSecret, claims)

	handler := WSAuthMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/ws/traces?token="+token, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("ws auth: status = %d, want 200", rec.Code)
	}
}

func TestWSAuthMiddleware_NoToken(t *testing.T) {
	handler := WSAuthMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/ws/traces", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("ws no token: status = %d, want 401", rec.Code)
	}
}

func TestWSAuthMiddleware_Disabled(t *testing.T) {
	handler := WSAuthMiddleware("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/ws/traces", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("ws auth disabled: status = %d, want 200", rec.Code)
	}
}
