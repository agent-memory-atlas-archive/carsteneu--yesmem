package httpapi

import (
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// LocalhostOnly rejects requests not from 127.0.0.1 or ::1.
func LocalhostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || (host != "127.0.0.1" && host != "::1") {
			http.Error(w, "forbidden: localhost only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BearerAuth validates Authorization: Bearer <token>. One implementation only:
// this delegates to BearerAuthFunc so the two wrappers cannot drift apart.
func BearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(BearerAuthFunc(token, next.ServeHTTP))
}

// BearerAuthFunc wraps a HandlerFunc with bearer token validation.
func BearerAuthFunc(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Without this an empty configured token authenticates "Bearer ": the
		// prefix matches and the remainder equals the empty token.
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "bearer "
		if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) ||
			subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// RequestLogger logs method, path, status, duration.
func RequestLogger(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		logger.Printf("[http] %s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}
