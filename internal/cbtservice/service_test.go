package cbtservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testID = "0123456789abcdef0123456789abcdef"

func TestMain(m *testing.M) {
	// Real subprocess test: this test binary can emulate the fixed CLI without
	// invoking a shell, touching a source disk or loading any kernel module.
	if os.Getenv("OPEN_VAULT_AGENT_TEST_HELPER") == "1" && len(os.Args) > 1 && os.Args[1] == "-config" {
		b, _ := os.ReadFile(os.Args[2])
		switch string(b) {
		case `{"helper":"hang"}`:
			time.Sleep(30 * time.Second)
		case `{"helper":"fail"}`:
			_, _ = os.Stderr.WriteString(`{"error":"SECRET-CONTENT /private/key"}`)
			os.Exit(1)
		case `{"helper":"overflow"}`:
			_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 5<<20))
			os.Exit(0)
		default:
			_, _ = os.Stdout.Write([]byte(successJSON()))
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func successJSON() string {
	return `{"generation":{"id":"` + testID + `","size":4096,"source_read_bytes":4096,"sequence":1},"this_run_source_read_bytes":1024}`
}

func secureFixtureDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		return t.TempDir()
	}
	// /tmp is intentionally world writable and forbidden for privileged startup
	// paths. Keep Linux fixtures under the current user's private cache instead.
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "open-vault-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func fixtureConfig(t *testing.T) Config {
	t.Helper()
	base := secureFixtureDir(t)
	binary := filepath.Join(base, "open-vault-cbt")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, input, 0700); err != nil {
		t.Fatal(err)
	}
	cliConfig := filepath.Join(base, "cli.json")
	if err := os.WriteFile(cliConfig, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{SocketPath: filepath.Join(base, "socket", "cbt.sock"), CLIBinary: binary, CLIConfig: cliConfig, StateDir: filepath.Join(base, "state"), RestoreRoot: filepath.Join(base, "images"), OperationTimeoutMinutes: 1}
}

func fixtureService(t *testing.T) *Service {
	t.Helper()
	s, err := New(context.Background(), fixtureConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.cancel(); s.wg.Wait() })
	return s
}

func TestGenerationHistoryUsesFixedCLIAndRejectsBrokenChain(t *testing.T) {
	s := fixtureService(t)
	for _, test := range []struct {
		body   string
		status int
	}{
		{`{"generations":[]}`, 200},
		{`{"generations":[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":4096,"source_read_bytes":4096}]}`, 200},
		{`{"generations":null}`, 502},
		{`{"generations":[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parent":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":4096}]}`, 502},
	} {
		s.execute = func(ctx context.Context, args ...string) ([]byte, []byte, error) {
			if !reflect.DeepEqual(args, []string{"-config", s.cfg.CLIConfig, "generations"}) {
				t.Fatalf("unsafe history argv: %v", args)
			}
			return []byte(test.body), nil, nil
		}
		w := request(s, "GET", "/generations", "")
		if w.Code != test.status {
			t.Fatalf("history response %s: %d", test.body, w.Code)
		}
	}
}

func request(s *Service, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	s.Handler().ServeHTTP(w, r)
	return w
}

func waitRun(t *testing.T, s *Service) Run {
	t.Helper()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runs) == 0 {
		t.Fatal("no run recorded")
	}
	return s.runs[0]
}

func TestAsyncSerializationAndFixedArguments(t *testing.T) {
	s := fixtureService(t)
	entered := make(chan []string, 1)
	release := make(chan struct{})
	s.execute = func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		entered <- args
		select {
		case <-release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return []byte(successJSON()), nil, nil
	}
	w := request(s, "POST", "/backup", "")
	if w.Code != 202 {
		t.Fatalf("expected accepted: %d %s", w.Code, w.Body.String())
	}
	args := <-entered
	if !reflect.DeepEqual(args, []string{"-config", s.cfg.CLIConfig, "backup"}) {
		t.Fatalf("unsafe argv: %v", args)
	}
	for _, path := range []string{"/backup", "/verify"} {
		if w := request(s, "POST", path, ""); w.Code != 409 {
			t.Fatalf("expected conflict for %s: %d", path, w.Code)
		}
	}
	if w := request(s, "GET", "/head", ""); w.Code != 409 {
		t.Fatal("status command overlapped backup")
	}
	if w := request(s, "GET", "/status", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"busy":true`) {
		t.Fatalf("wrong status: %s", w.Body.String())
	}
	close(release)
	run := waitRun(t, s)
	if run.Status != "succeeded" || run.GenerationID != testID || run.SourceReadBytes != 1024 || run.FinishedAt == nil {
		t.Fatalf("wrong run: %+v", run)
	}
	if w := request(s, "GET", "/runs", ""); w.Code != 200 || !strings.Contains(w.Body.String(), run.ID) {
		t.Fatal("missing run API")
	}
}

func TestRestoreBoundariesAndFixedFlags(t *testing.T) {
	s := fixtureService(t)
	var args []string
	s.execute = func(_ context.Context, a ...string) ([]byte, []byte, error) {
		args = append([]string{}, a...)
		return []byte(successJSON()), nil, nil
	}
	for _, name := range []string{"..", "../escape", "a/b", "a\\b", "x.img", "", "--x;touch"} {
		body, _ := json.Marshal(map[string]string{"generation": testID, "target_name": name})
		if w := request(s, "POST", "/restore", string(body)); w.Code != 400 {
			t.Fatalf("accepted restore name %q: %d", name, w.Code)
		}
	}
	for _, id := range []string{"latest", "1234", strings.Repeat("A", 32), "--help"} {
		body, _ := json.Marshal(map[string]string{"generation": id, "target_name": "restore"})
		if w := request(s, "POST", "/restore", string(body)); w.Code != 400 {
			t.Fatalf("accepted generation %q", id)
		}
	}
	if w := request(s, "POST", "/restore", `{"generation":"`+testID+`","target_name":"restore-1"}`); w.Code != 202 {
		t.Fatalf("restore failed: %d %s", w.Code, w.Body.String())
	}
	run := waitRun(t, s)
	target := filepath.Join(s.cfg.RestoreRoot, "restore-1.img")
	want := []string{"-config", s.cfg.CLIConfig, "-generation", testID, "-target", target, "restore"}
	if !reflect.DeepEqual(args, want) || run.Target != target {
		t.Fatalf("incorrect restore: %v %+v", args, run)
	}
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := request(s, "POST", "/restore", `{"generation":"`+testID+`","target_name":"restore-1"}`); w.Code != 409 {
		t.Fatal("accepted existing target")
	}
}

func TestHeadEmptyVersusFailureAndSanitization(t *testing.T) {
	s := fixtureService(t)
	s.execute = func(context.Context, ...string) ([]byte, []byte, error) {
		return nil, []byte(`{"error":"repository has no generations"}`), errors.New("exit 1")
	}
	if w := request(s, "GET", "/head", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"generation":null`) {
		t.Fatalf("empty repository: %d %s", w.Code, w.Body.String())
	}
	s.execute = func(context.Context, ...string) ([]byte, []byte, error) {
		return nil, []byte(`{"error":"SECRET /private/password"}`), errors.New("SECRET")
	}
	if w := request(s, "GET", "/head", ""); w.Code != 502 || strings.Contains(w.Body.String(), "SECRET") || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("leaked error or hid failure: %d %s", w.Code, w.Body.String())
	}
	s.execute = func(context.Context, ...string) ([]byte, []byte, error) { return []byte(successJSON()), nil, nil }
	if w := request(s, "GET", "/head", ""); w.Code != 200 || !strings.Contains(w.Body.String(), testID) {
		t.Fatalf("head failed: %s", w.Body.String())
	}
}

func TestRunPersistenceAndInterruptedRecovery(t *testing.T) {
	s := fixtureService(t)
	s.execute = func(context.Context, ...string) ([]byte, []byte, error) { return []byte(successJSON()), nil, nil }
	if w := request(s, "POST", "/verify", `{"generation":"`+testID+`"}`); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	completed := waitRun(t, s)
	s.mu.Lock()
	s.runs = append([]Run{{ID: strings.Repeat("a", 32), Kind: "backup", Status: "running", StartedAt: time.Now().UTC()}}, s.runs...)
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(context.Background(), s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.cancel()
	if len(restarted.runs) != 2 || restarted.runs[0].Status != "failed" || restarted.runs[0].FinishedAt == nil || restarted.runs[1].ID != completed.ID {
		t.Fatalf("recovery failed: %+v", restarted.runs)
	}
	info, err := os.Stat(s.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("history permissions are not private")
	}
}

func TestFailedPublishedRunKeepsOnlySafeMetadata(t *testing.T) {
	s := fixtureService(t)
	s.execute = func(context.Context, ...string) ([]byte, []byte, error) {
		return nil, []byte(`{"committed":true,"generation":{"id":"` + testID + `","size":4096,"source_read_bytes":4096},"error":"SECRET /private/key"}`), errors.New("exit")
	}
	if w := request(s, "POST", "/backup", ""); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	r := waitRun(t, s)
	if r.Status != "failed" || r.GenerationID != testID || strings.Contains(r.Error, "SECRET") {
		t.Fatalf("wrong failed metadata: %+v", r)
	}
}

func TestInvalidPayloadsNeverExecute(t *testing.T) {
	s := fixtureService(t)
	s.execute = func(context.Context, ...string) ([]byte, []byte, error) {
		t.Error("invalid request executed CLI")
		return nil, nil, errors.New("invalid")
	}
	for _, tc := range []struct{ path, body string }{
		{"/backup", `{"command":"destroy"}`}, {"/verify", `{"generation":"--help"}`}, {"/restore", `{}`}, {"/restore", `{"generation":"` + testID + `","target_name":"ok"} {}`}, {"/backup", strings.Repeat("x", 5000)},
	} {
		if w := request(s, "POST", tc.path, tc.body); w.Code != 400 {
			t.Fatalf("accepted malformed payload %s: %d", tc.path, w.Code)
		}
	}
}

func TestRealSubprocessTimeoutFailureAndOutputLimit(t *testing.T) {
	t.Setenv("OPEN_VAULT_AGENT_TEST_HELPER", "1")
	s := fixtureService(t)
	for _, kind := range []string{"hang", "fail", "overflow"} {
		t.Run(kind, func(t *testing.T) {
			if err := os.WriteFile(s.cfg.CLIConfig, []byte(`{"helper":"`+kind+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			s.timeout = 150 * time.Millisecond
			if kind == "overflow" {
				s.timeout = 5 * time.Second
			}
			before := time.Now()
			if w := request(s, "POST", "/backup", ""); w.Code != 202 {
				t.Fatal(w.Body.String())
			}
			r := waitRun(t, s)
			if r.Status != "failed" || strings.Contains(r.Error, "SECRET") || strings.Contains(r.Error, "private") {
				t.Fatalf("unsafe subprocess outcome: %+v", r)
			}
			if kind == "hang" && time.Since(before) > 5*time.Second {
				t.Fatal("subprocess was not killed on timeout")
			}
		})
	}
}

func TestStartupRejectsInsecurePathsAndRestoreOverlap(t *testing.T) {
	for _, kind := range []string{"relative", "binary_writable", "config_public", "state_public", "ancestor_writable", "restore_source", "restore_state"} {
		t.Run(kind, func(t *testing.T) {
			c := fixtureConfig(t)
			switch kind {
			case "relative":
				c.SocketPath = "relative.sock"
			case "binary_writable":
				if runtime.GOOS == "windows" {
					t.Skip("Unix permissions")
				}
				_ = os.Chmod(c.CLIBinary, 0770)
			case "config_public":
				if runtime.GOOS == "windows" {
					t.Skip("Unix permissions")
				}
				_ = os.Chmod(c.CLIConfig, 0644)
			case "state_public":
				if runtime.GOOS == "windows" {
					t.Skip("Unix permissions")
				}
				_ = os.Mkdir(c.StateDir, 0777)
				_ = os.Chmod(c.StateDir, 0777)
			case "ancestor_writable":
				if runtime.GOOS != "linux" {
					t.Skip("Linux ownership boundary")
				}
				_ = os.Chmod(filepath.Dir(c.CLIBinary), 0770)
			case "restore_source":
				b, _ := json.Marshal(map[string]any{"volume": map[string]string{"mount_point": c.RestoreRoot}})
				_ = os.WriteFile(c.CLIConfig, b, 0600)
			case "restore_state":
				c.RestoreRoot = c.StateDir
			}
			if s, err := New(context.Background(), c); err == nil {
				s.cancel()
				t.Fatal("accepted unsafe startup configuration")
			}
		})
	}
}

func TestUnixSocketPermissionsAndRefuseExistingPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		s := fixtureService(t)
		if err := s.Serve(context.Background()); err == nil {
			t.Fatal("unsupported socket service started")
		}
		return
	}
	s := fixtureService(t)
	if err := os.WriteFile(s.cfg.SocketPath, []byte("do not delete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(context.Background()); err == nil {
		t.Fatal("deleted existing socket path")
	}
	b, err := os.ReadFile(s.cfg.SocketPath)
	if err != nil || string(b) != "do not delete" {
		t.Fatal("existing path changed")
	}
	if err := os.Remove(s.cfg.SocketPath); err != nil {
		t.Fatal(err)
	}
	s2, err := New(context.Background(), s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.cancel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- s2.Serve(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		info, err := os.Stat(s2.cfg.SocketPath)
		if err == nil && info.Mode().Perm() == 0660 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent socket did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s2.cfg.SocketPath)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	resp, err := client.Get("http://agent/status")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"configured":true`) {
		t.Fatalf("socket API failed: %s", data)
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("socket shutdown hung")
	}
	if _, err := os.Lstat(s2.cfg.SocketPath); !os.IsNotExist(err) {
		t.Fatal("owned socket not cleaned up")
	}
}

func TestLoadConfigRejectsUnknownAndPublicConfiguration(t *testing.T) {
	c := fixtureConfig(t)
	path := filepath.Join(filepath.Dir(c.CLIConfig), "agent.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil || got.CLIBinary != c.CLIBinary {
		t.Fatalf("config load failed: %+v %v", got, err)
	}
	if err := os.WriteFile(path, []byte(`{"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("accepted unknown agent configuration")
	}
}
