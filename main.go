package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var assets embed.FS

const version = "0.2.0-alpha.2"

// The web controller has no repository, device, driver or encryption key access.
type Config struct {
	Listen    string `json:"listen"`
	TokenFile string `json:"token_file"`
	CBTSocket string `json:"cbt_socket"`
}

type Server struct {
	cfg   Config
	token string
}

func readSecret(path string) (string, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
		return "", errors.New("API token must be a private regular file (0600), not a symlink")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("cannot read API token")
	}
	return strings.TrimSpace(string(b)), nil
}

func loadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return c, errors.New("invalid web configuration; use the CBT-only config.example.json")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return c, errors.New("expected one configuration object")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return c, err
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return c, errors.New("listen must be loopback; use a reverse proxy for remote access")
	}
	if !filepath.IsAbs(c.TokenFile) || !filepath.IsAbs(c.CBTSocket) {
		return c, errors.New("token_file and cbt_socket require absolute paths")
	}
	return c, nil
}

func newServer(c Config) (*Server, error) {
	token, err := readSecret(c.TokenFile)
	if err != nil {
		return nil, err
	}
	if len(token) < 32 {
		return nil, errors.New("API token must contain at least 32 characters")
	}
	return &Server{cfg: c, token: token}, nil
}

func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, message string) {
	jsonOut(w, status, map[string]string{"error": message})
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/api/", s.api)
	sub, _ := fs.Sub(assets, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(supplied), []byte(s.token)) != 1 {
		fail(w, 401, "valid Bearer token required")
		return
	}
	if r.Method == "GET" && r.URL.Path == "/api/status" {
		jsonOut(w, 200, map[string]any{"version": version, "mode": "cbt", "configured": s.cfg.CBTSocket != ""})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/cbt/") {
		s.cbtAPI(w, r)
		return
	}
	fail(w, 404, "API route not found")
}

func main() {
	path := flag.String("config", "config.json", "configuration file")
	flag.Parse()
	cfg, err := loadConfig(*path)
	if err != nil {
		log.Fatal(err)
	}
	s, err := newServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: cfg.Listen, Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("Open Vault %s CBT console listening at http://%s", version, cfg.Listen)
	if err = srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
