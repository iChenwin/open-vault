package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ichenwin/open-vault/internal/engine"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeEngine struct {
	block chan struct{}
	fail  bool
}

func TestRealAPIBackupRestore(t *testing.T) {
	binary := os.Getenv("RESTIC_TEST_BINARY")
	if binary == "" {
		t.Skip("RESTIC_TEST_BINARY is not configured")
	}
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	password := filepath.Join(base, "password")
	if err := os.WriteFile(password, []byte("integration-test-only-password"), 0600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(base, "token")
	if err := os.WriteFile(token, []byte(strings.Repeat("q", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	payload := []byte("Original content saved through the HTTP API\n")
	file := filepath.Join(source, "business.txt")
	if err := os.WriteFile(file, payload, 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{StateDir: filepath.Join(base, "state"), TokenFile: token, SourceRoots: []string{source}, RestoreRoot: filepath.Join(base, "restore"), Repository: filepath.Join(base, "repo"), PasswordFile: password, Timeout: 2}
	e, err := engine.New(engine.Config{Binary: binary, Repository: c.Repository, PasswordFile: password, RestoreRoot: c.RestoreRoot, SourceRoots: c.SourceRoots})
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(context.Background(), c, e)
	if err != nil {
		t.Fatal(err)
	}
	run := func(path, body string) {
		t.Helper()
		w := request(s, "POST", path, body, s.token)
		if w.Code != 202 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		s.wg.Wait()
		if s.state.Runs[0].Status != "succeeded" {
			t.Fatalf("operation failed: %+v", s.state.Runs[0])
		}
	}
	run("/api/init", "{}")
	body, _ := json.Marshal(map[string]any{"name": "real-linux", "paths": []string{source}, "interval_minutes": 60, "enabled": false})
	w := request(s, "POST", "/api/jobs", string(body), s.token)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var job Job
	if err = json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	run("/api/jobs/"+job.ID+"/run", "{}")
	w = request(s, "GET", "/api/snapshots", "", s.token)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var snapshots []struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &snapshots); err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots: %s %v", w.Body.String(), err)
	}
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	run("/api/check", "{}")
	run("/api/restore", `{"snapshot":"`+snapshots[0].ID+`","target_name":"api-recovery"}`)
	found := false
	err = filepath.WalkDir(s.state.Runs[0].Target, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == "business.txt" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if string(data) != string(payload) {
				t.Error("restored content mismatch")
			}
			found = true
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("restored file missing: %v", err)
	}
}

func (f *fakeEngine) Init(ctx context.Context) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.fail {
		return errors.New("test engine failed")
	}
	return nil
}
func (f *fakeEngine) Backup(c context.Context, p []string, t string) error { return f.Init(c) }
func (f *fakeEngine) Check(c context.Context) error                        { return f.Init(c) }
func (f *fakeEngine) Snapshots(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}
func (f *fakeEngine) Restore(c context.Context, s, t string) (string, error) {
	return "/restore/" + t, f.Init(c)
}
func (f *fakeEngine) ValidateSources(p []string) error {
	if len(p) == 0 {
		return errors.New("empty paths")
	}
	return nil
}
func testServer(t *testing.T, e backend) *Server {
	t.Helper()
	base := t.TempDir()
	token := filepath.Join(base, "token")
	if err := os.WriteFile(token, []byte(strings.Repeat("t", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{StateDir: filepath.Join(base, "state"), TokenFile: token, SourceRoots: []string{filepath.Join(base, "source")}, RestoreRoot: filepath.Join(base, "restore"), Timeout: 1}
	s, err := newServer(context.Background(), c, e)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func request(s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}
func TestAuthenticationAndInput(t *testing.T) {
	s := testServer(t, &fakeEngine{})
	tok := s.token
	bare := httptest.NewRequest("GET", "/api/status", nil)
	bare.Header.Set("Authorization", tok)
	bareResult := httptest.NewRecorder()
	s.handler().ServeHTTP(bareResult, bare)
	if bareResult.Code != 401 {
		t.Fatal("Bearer scheme not enforced")
	}
	for _, path := range []string{"/api/jobs", "/api/runs", "/api/snapshots", "/api/status"} {
		if w := request(s, "GET", path, "", ""); w.Code != 401 {
			t.Fatalf("unprotected %s", path)
		}
	}
	if w := request(s, "GET", "/healthz", "", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`{}`, `{"name":"x","paths":[],"interval_minutes":1}`, `{"name":"x","paths":["/data"],"interval_minutes":1,"unknown":true}`, `{} {}`} {
		if w := request(s, "POST", "/api/jobs", body, tok); w.Code != 400 {
			t.Fatalf("accepted %s: %d", body, w.Code)
		}
	}
}

func TestCredentialSymlinkBoundary(t *testing.T) {
	s := testServer(t, &fakeEngine{})
	alias := filepath.Join(t.TempDir(), "source-alias")
	if err := os.Symlink(filepath.Dir(s.cfg.TokenFile), alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	c := s.cfg
	c.SourceRoots = []string{alias}
	if _, err := newServer(context.Background(), c, &fakeEngine{}); err == nil {
		t.Fatal("source alias exposed API token or state")
	}
}
func TestJobLifecycleAndPersistence(t *testing.T) {
	s := testServer(t, &fakeEngine{})
	tok := s.token
	w := request(s, "POST", "/api/jobs", `{"name":"data","paths":["/data"],"interval_minutes":60,"enabled":true}`, tok)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var j Job
	_ = json.Unmarshal(w.Body.Bytes(), &j)
	w = request(s, "PATCH", "/api/jobs/"+j.ID, `{"enabled":false}`, tok)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	s2, e := newServer(context.Background(), s.cfg, &fakeEngine{})
	if e != nil {
		t.Fatal(e)
	}
	if len(s2.state.Jobs) != 1 || s2.state.Jobs[0].Enabled {
		t.Fatal("job not persisted")
	}
	w = request(s, "DELETE", "/api/jobs/"+j.ID, "", tok)
	if w.Code != 204 || len(s.state.Jobs) != 0 {
		t.Fatal("delete failed")
	}
}
func TestBusyFailureAndRecovery(t *testing.T) {
	f := &fakeEngine{block: make(chan struct{}), fail: true}
	s := testServer(t, f)
	if _, code, e := s.start("init", "", "", ""); e != nil || code != 202 {
		t.Fatal(e)
	}
	if _, code, _ := s.start("check", "", "", ""); code != 409 {
		t.Fatal("concurrent operation allowed")
	}
	if w := request(s, "GET", "/api/snapshots", "", s.token); w.Code != 409 {
		t.Fatal("snapshots raced writer")
	}
	close(f.block)
	s.wg.Wait()
	if s.state.Runs[0].Status != "failed" || s.busy {
		t.Fatal("failure state missing")
	}
	s.state.Runs[0].Status = "running"
	if e := s.save(); e != nil {
		t.Fatal(e)
	}
	recovered, e := newServer(context.Background(), s.cfg, &fakeEngine{})
	if e != nil {
		t.Fatal(e)
	}
	if recovered.state.Runs[0].Status != "failed" {
		t.Fatal("interrupted run not recovered")
	}
}
func TestSchedulerHonorsDisabledAndInterval(t *testing.T) {
	s := testServer(t, &fakeEngine{})
	s.state.Jobs = []Job{{ID: "disabled", Enabled: false, Interval: 1}, {ID: "future", Enabled: true, Interval: 60, LastAttempt: time.Now()}}
	s.schedule(time.Now())
	if len(s.state.Runs) != 0 {
		t.Fatal("ineligible jobs ran")
	}
	s.state.Jobs = append(s.state.Jobs, Job{ID: "due", Enabled: true, Interval: 1})
	s.schedule(time.Now())
	s.wg.Wait()
	if len(s.state.Runs) != 1 || s.state.Runs[0].JobID != "due" {
		t.Fatal("due job missing")
	}
	s.schedule(time.Now())
	s.wg.Wait()
	if len(s.state.Runs) != 1 {
		t.Fatal("job retriggered before interval")
	}
}
