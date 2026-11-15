package main

import (
	"crypto/subtle"
	"encoding/base64"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Cloud mode is opt-in and requires an administrator password. Credentials
// are supplied through the hosting dashboard, never committed into the code.
func cloudMode() bool { return os.Getenv("CLOUD_MODE") == "1" }

func adminPassword() string { return os.Getenv("APP_PASSWORD") }

func secureEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func cloudAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		if r.URL.Path == "/api/stop" || r.URL.Path == "/api/phone-link" {
			http.NotFound(w, r)
			return
		}
		// The hosting provider terminates TLS and forwards it to this app.
		// The public URL must only be accessed through HTTPS.
		if r.Header.Get("X-Forwarded-Proto") != "https" && r.TLS == nil && os.Getenv("CLOUD_ALLOW_HTTP") != "1" {
			http.Error(w, "HTTPS required", http.StatusUpgradeRequired)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || !secureEqual(u, "admin") || !secureEqual(p, adminPassword()) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Pulse Agent (private)"`)
			http.Error(w, "Авторизация требуется", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	if !cloudMode() {
		log.Fatal("Эта версия предназначена для облачного запуска: CLOUD_MODE=1")
	}
	if len(adminPassword()) < 16 {
		log.Fatal("Перед запуском задай APP_PASSWORD длиной от 16 символов через переменные окружения хостинга")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}
	app := newApp()
	mux := http.NewServeMux()
	app.routes(mux)
	go app.loop()
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           cloudAuth(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       70 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	log.Print("Pulse Agent mobile cloud: listening on port ", port)
	log.Fatal(srv.ListenAndServe())
}

// Helper for clients that request Authorization: Basic <...>; retained here
// only to keep test utilities deterministic.
func basicValue(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(strings.Join([]string{user, password}, ":")))
}
