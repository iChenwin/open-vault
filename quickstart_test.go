package main

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOccupiedPortFallbackAndExplicitPort(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	original := occupied.Addr().String()
	fixed := Config{Listen: original}
	if listener, err := listenConsole(&fixed, false); err == nil {
		listener.Close()
		t.Fatal("explicit occupied port should fail")
	}
	if fixed.Listen != original {
		t.Fatal("explicit address changed")
	}
	if occupied.Addr().(*net.TCPAddr).Port > 65525 {
		t.Skip("ephemeral port too close to range end for fallback test")
	}
	automatic := Config{Listen: original}
	listener, err := listenConsole(&automatic, true)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host, text, _ := net.SplitHostPort(automatic.Listen)
	port, _ := strconv.Atoi(text)
	base := occupied.Addr().(*net.TCPAddr).Port
	if host != "127.0.0.1" || port <= base || port > base+10 {
		t.Fatal("fallback escaped its host or bounded range", automatic.Listen)
	}
	connection, err := net.Dial("tcp", automatic.Listen)
	if err != nil {
		t.Fatal("reported address is not bound", err)
	}
	connection.Close()
}

func TestQuickstartPreservesTokenAndExistingConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "settings")
	cfg, path, err := prepareQuickstart(dir)
	if err != nil {
		t.Fatal(err)
	}
	token, err := readSecret(cfg.TokenFile)
	if err != nil || len(token) != 64 {
		t.Fatal("invalid generated token", err)
	}
	cfg.Listen = "127.0.0.1:8099"
	data, _ := json.Marshal(cfg)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	second, secondPath, err := prepareQuickstart(dir)
	if err != nil || secondPath != path || second.Listen != cfg.Listen {
		t.Fatal("existing settings not preserved", second, err)
	}
	again, err := readSecret(second.TokenFile)
	if err != nil || again != token {
		t.Fatal("existing credential changed", err)
	}
	if err = os.WriteFile(second.TokenFile, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = newServer(second); err == nil {
		t.Fatal("invalid existing credential accepted")
	}
}

func TestLANConfigAndPeerBoundary(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Listen: "0.0.0.0:8080", TokenFile: filepath.Join(dir, "token"), CBTSocket: filepath.Join(dir, "agent.sock")}
	path := filepath.Join(dir, "config.json")
	for _, allow := range []bool{false, true} {
		cfg.AllowLAN = allow
		data, _ := json.Marshal(cfg)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfig(path)
		if (err == nil) != allow {
			t.Fatalf("allow_lan=%v: %v", allow, err)
		}
	}
	cfg.Listen = "8.8.8.8:8080"
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile(path, data, 0600)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("public IP bind accepted")
	}
	s := &Server{cfg: Config{AllowLAN: true}, token: strings.Repeat("a", 64)}
	for _, test := range []struct {
		peer    string
		allowed bool
	}{
		{"192.168.1.8:50000", true}, {"10.0.0.3:1234", true},
		{"172.16.0.2:1234", true}, {"127.0.0.1:1234", true},
		{"[::1]:1234", true}, {"[fd00::2]:1234", true},
		{"203.0.113.3:1234", false}, {"[2001:4860:4860::8888]:1234", false},
		{"invalid", false},
	} {
		for _, authenticated := range []bool{false, true} {
			r := httptest.NewRequest("GET", "/api/status", nil)
			r.RemoteAddr = test.peer
			// Forwarded headers cannot turn a public peer into a LAN client.
			r.Header.Set("X-Forwarded-For", "192.168.1.2")
			if authenticated {
				r.Header.Set("Authorization", "Bearer "+s.token)
			}
			w := httptest.NewRecorder()
			s.handler().ServeHTTP(w, r)
			want := 403
			if test.allowed {
				want = 401
				if authenticated {
					want = 200
				}
			}
			if w.Code != want {
				t.Fatalf("peer %s auth %v: %d want %d", test.peer, authenticated, w.Code, want)
			}
		}
	}
}
