package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCBTConsoleAuthenticationAndRemovedRoutes(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("q", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(Config{TokenFile: tokenPath})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path  string
		authenticated bool
		status        int
	}{
		{"GET", "/", false, 200},
		{"GET", "/healthz", false, 200},
		{"GET", "/api/status", false, 401},
		{"POST", "/api/cbt/backup", false, 401},
		{"GET", "/api/status", true, 200},
		{"GET", "/api/jobs", true, 404},
		{"GET", "/api/snapshots", true, 404},
		{"POST", "/api/init", true, 404},
		{"POST", "/api/restore", true, 404},
	} {
		r := httptest.NewRequest(test.method, test.path, nil)
		if test.authenticated {
			r.Header.Set("Authorization", "Bearer "+s.token)
		}
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s: %d, want %d", test.path, w.Code, test.status)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing cache protection")
		}
	}
}

func TestCBTOnlyConfigRejectsObsoleteSettings(t *testing.T) {
	dir := t.TempDir()
	c := Config{Listen: "127.0.0.1:8080", TokenFile: filepath.Join(dir, "token"), CBTSocket: filepath.Join(dir, "agent.sock")}
	path := filepath.Join(dir, "config.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	fields["repository"] = filepath.Join(dir, "old-repository")
	b, _ = json.Marshal(fields)
	_ = os.WriteFile(path, b, 0600)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("obsolete config silently accepted")
	}
	c.Listen = "0.0.0.0:8080"
	b, _ = json.Marshal(c)
	_ = os.WriteFile(path, b, 0600)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("unexpected public listener")
	}
}
