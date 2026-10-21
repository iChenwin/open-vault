package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func cbtProxyRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	s.cbtAPI(w, r)
	return w
}

func fakeCBTUnixDaemon(t *testing.T, h http.HandlerFunc) *Server {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket daemon integration runs on Linux")
	}
	path := filepath.Join(t.TempDir(), "cbt.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: h}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return &Server{cfg: Config{CBTSocket: path}}
}

func TestCBTProxyUnconfiguredAndAllowlist(t *testing.T) {
	s := &Server{}
	w := cbtProxyRequest(s, "GET", "/api/cbt/status", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":false`) || !strings.Contains(w.Body.String(), `"busy":false`) {
		t.Fatalf("bad unconfigured status: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"runs", "head", "backup", "verify", "restore"} {
		method := "GET"
		if path == "backup" || path == "verify" || path == "restore" {
			method = "POST"
		}
		if got := cbtProxyRequest(s, method, "/api/cbt/"+path, "{}"); got.Code != 503 {
			t.Fatalf("unconfigured %s: %d", path, got.Code)
		}
	}
	for _, path := range []string{"/api/cbt/exec", "/api/cbt/../backup", "/api/cbt/status/", "/api/cbt/http://example.com"} {
		if got := cbtProxyRequest(s, "GET", path, ""); got.Code != 404 {
			t.Fatalf("unknown path accepted %q: %d", path, got.Code)
		}
	}
	if got := cbtProxyRequest(s, "DELETE", "/api/cbt/backup", ""); got.Code != 405 || got.Header().Get("Allow") != "POST" {
		t.Fatal("wrong method accepted")
	}
	if got := cbtProxyRequest(s, "GET", "/api/cbt/status?url=http://example.com", ""); got.Code != 400 {
		t.Fatal("query accepted")
	}
}

func TestCBTProxyUsesExistingAPIToken(t *testing.T) {
	s := &Server{token: "existing-file-backup-token"}
	for _, token := range []string{"", "Bearer wrong", "Bearer existing-file-backup-token"} {
		r := httptest.NewRequest("GET", "/api/cbt/status", nil)
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		s.api(w, r)
		want := http.StatusUnauthorized
		if token == "Bearer existing-file-backup-token" {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Fatalf("token %q returned %d, want %d", token, w.Code, want)
		}
	}
}

func TestCBTProxyUnixConfinementAndHeaders(t *testing.T) {
	type received struct{ Method, Path, Host, Auth, Cookie, Forwarded, ContentType, Body string }
	requests := make(chan received, 4)
	s := fakeCBTUnixDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		requests <- received{r.Method, r.URL.Path, r.Host, r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("X-Forwarded-Host"), r.Header.Get("Content-Type"), string(b)}
		w.Header().Set("Set-Cookie", "privileged=secret")
		w.Header().Set("X-Root-Secret", "secret")
		if r.Method == "POST" {
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"run-1"}`))
		} else {
			_, _ = w.Write([]byte(`{"configured":true,"busy":false}`))
		}
	})
	r := httptest.NewRequest("GET", "http://attacker.invalid/api/cbt/status", nil)
	r.Header.Set("Authorization", "Bearer browser-secret")
	r.Header.Set("Cookie", "browser=secret")
	r.Header.Set("X-Forwarded-Host", "attacker.invalid")
	w := httptest.NewRecorder()
	s.cbtAPI(w, r)
	if w.Code != 200 || w.Header().Get("Set-Cookie") != "" || w.Header().Get("X-Root-Secret") != "" {
		t.Fatalf("unsafe response forwarding: %d %+v", w.Code, w.Header())
	}
	got := <-requests
	if got.Host != "cbt.local" || got.Path != "/status" || got.Auth != "" || got.Cookie != "" || got.Forwarded != "" {
		t.Fatalf("incoming request escaped fixed upstream: %+v", got)
	}
	body := `{"generation":"0123456789abcdef0123456789abcdef","target_name":"restore_01"}`
	w = cbtProxyRequest(s, "POST", "/api/cbt/restore", body)
	if w.Code != 202 {
		t.Fatalf("restore proxy: %d %s", w.Code, w.Body.String())
	}
	got = <-requests
	if got.Method != "POST" || got.Path != "/restore" || got.ContentType != "application/json" || got.Body != body {
		t.Fatalf("bad restore request: %+v", got)
	}
	w = cbtProxyRequest(s, "POST", "/api/cbt/verify", `{"generation":"0123456789abcdef0123456789abcdef"}`)
	if w.Code != 202 {
		t.Fatal("verify generation rejected")
	}
	<-requests
	// Exactly 64 KiB is accepted, normalized to the only permitted backup object.
	w = cbtProxyRequest(s, "POST", "/api/cbt/backup", strings.Repeat(" ", cbtRequestLimit-2)+"{}")
	if w.Code != 202 {
		t.Fatalf("boundary body rejected: %d", w.Code)
	}
	if got = <-requests; got.Body != "{}" {
		t.Fatal("backup body was not normalized")
	}
}

func TestCBTProxyRejectsUnsafeAndOversizedBodies(t *testing.T) {
	s := fakeCBTUnixDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid request reached root daemon")
		_, _ = w.Write([]byte(`{}`))
	})
	for _, body := range []string{
		`{"generation":"0123456789abcdef0123456789abcdef","target_name":"../escape"}`,
		`{"generation":"short","target_name":"safe"}`,
		`{"generation":"0123456789abcdef0123456789abcdef","target_name":"/dev/sda"}`,
		`{"generation":"0123456789abcdef0123456789abcdef","target_name":"safe","target":"/dev/sda"}`,
		`{"generation":"0123456789abcdef0123456789abcdef","target_name":"safe"} {}`,
		`null`, `[]`, `broken`,
	} {
		if w := cbtProxyRequest(s, "POST", "/api/cbt/restore", body); w.Code != 400 {
			t.Fatalf("unsafe restore body accepted: %s: %d", body, w.Code)
		}
	}
	if w := cbtProxyRequest(s, "POST", "/api/cbt/backup", `{"command":"exec"}`); w.Code != 400 {
		t.Fatal("arbitrary backup object accepted")
	}
	if w := cbtProxyRequest(s, "GET", "/api/cbt/status", `{}`); w.Code != 400 {
		t.Fatal("GET body accepted")
	}
	if w := cbtProxyRequest(s, "POST", "/api/cbt/backup", strings.Repeat(" ", cbtRequestLimit+1)); w.Code != 413 {
		t.Fatal("large declared body accepted")
	}
	r := httptest.NewRequest("POST", "/api/cbt/backup", bytes.NewReader(bytes.Repeat([]byte(" "), cbtRequestLimit+1)))
	r.ContentLength = -1
	w := httptest.NewRecorder()
	s.cbtAPI(w, r)
	if w.Code != 413 {
		t.Fatal("large streamed body accepted")
	}
}

func TestCBTProxyErrorsAndResponseValidation(t *testing.T) {
	s := fakeCBTUnixDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backup":
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"busy"}`))
		case "/status":
			w.Header().Set("Location", "http://example.com")
			w.WriteHeader(302)
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte("<html>invalid</html>"))
		}
	})
	w := cbtProxyRequest(s, "POST", "/api/cbt/backup", "{}")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "busy") {
		t.Fatal("lost busy conflict")
	}
	if w = cbtProxyRequest(s, "GET", "/api/cbt/status", ""); w.Code != 502 {
		t.Fatal("redirect accepted or followed")
	}
	if w = cbtProxyRequest(s, "GET", "/api/cbt/runs", ""); w.Code != 502 || !json.Valid(w.Body.Bytes()) {
		t.Fatal("non-JSON response forwarded")
	}
	s.cfg.CBTSocket = filepath.Join(t.TempDir(), "missing.sock")
	if w = cbtProxyRequest(s, "GET", "/api/cbt/status", ""); w.Code != 503 {
		t.Fatal("unavailable daemon not isolated")
	}
}

func TestCBTProxyDeadlineDoesNotReportAnOperationFailure(t *testing.T) {
	s := fakeCBTUnixDaemon(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r := httptest.NewRequest("POST", "/api/cbt/backup", strings.NewReader("{}")).WithContext(ctx)
	w := httptest.NewRecorder()
	s.cbtAPI(w, r)
	if w.Code != http.StatusGatewayTimeout || !strings.Contains(w.Body.String(), "may already be accepted") {
		t.Fatalf("ambiguous timeout not explained: %d %s", w.Code, w.Body.String())
	}
}
