package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	"sync"
	"syscall"
	"time"

	"github.com/ichenwin/open-vault/internal/engine"
)

//go:embed web/*
var assets embed.FS

const version = "0.2.0-alpha.1"

type Config struct {
	Listen       string   `json:"listen"`
	StateDir     string   `json:"state_dir"`
	TokenFile    string   `json:"token_file"`
	Repository   string   `json:"repository"`
	PasswordFile string   `json:"password_file"`
	RestoreRoot  string   `json:"restore_root"`
	SourceRoots  []string `json:"source_roots"`
	ResticBinary string   `json:"restic_binary"`
	CBTSocket    string   `json:"cbt_socket,omitempty"`
	Timeout      int      `json:"operation_timeout_minutes"`
}
type Job struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Paths       []string  `json:"paths"`
	Interval    int       `json:"interval_minutes"`
	Enabled     bool      `json:"enabled"`
	LastAttempt time.Time `json:"last_attempt"`
}
type Run struct {
	ID       string     `json:"id"`
	JobID    string     `json:"job_id"`
	Kind     string     `json:"kind"`
	Status   string     `json:"status"`
	Started  time.Time  `json:"started_at"`
	Finished *time.Time `json:"finished_at,omitempty"`
	Error    string     `json:"error,omitempty"`
	Target   string     `json:"target,omitempty"`
}
type State struct {
	Jobs []Job `json:"jobs"`
	Runs []Run `json:"runs"`
}
type backend interface {
	Init(context.Context) error
	Backup(context.Context, []string, string) error
	Snapshots(context.Context) (json.RawMessage, error)
	Restore(context.Context, string, string) (string, error)
	Check(context.Context) error
	ValidateSources([]string) error
}
type Server struct {
	mu     sync.Mutex
	cfg    Config
	token  string
	engine backend
	state  State
	busy   bool
	ctx    context.Context
	wg     sync.WaitGroup
}

func uid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func readSecret(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", errors.New("cannot read secret file")
	}
	if !st.Mode().IsRegular() || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
		return "", errors.New("secret must be a regular file with mode 0600")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("cannot read secret file")
	}
	return strings.TrimSpace(string(b)), nil
}
func loadConfig(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, e
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.Timeout == 0 {
		c.Timeout = 120
	}
	if c.Timeout < 1 || c.Timeout > 10080 {
		return c, errors.New("operation timeout must be 1..10080 minutes")
	}
	host, _, e := net.SplitHostPort(c.Listen)
	if e != nil {
		return c, e
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return c, errors.New("listen must be loopback; use an authenticated TLS reverse proxy for remote access")
	}
	for _, p := range []string{c.StateDir, c.TokenFile, c.PasswordFile, c.Repository, c.RestoreRoot} {
		if !filepath.IsAbs(p) {
			return c, errors.New("config paths must be absolute")
		}
	}
	if len(c.SourceRoots) == 0 {
		return c, errors.New("source_roots is required")
	}
	if c.CBTSocket != "" && !filepath.IsAbs(c.CBTSocket) {
		return c, errors.New("cbt_socket must be an absolute Unix socket path")
	}
	return c, nil
}
func newServer(ctx context.Context, c Config, e backend) (*Server, error) {
	token, err := readSecret(c.TokenFile)
	if err != nil {
		return nil, err
	}
	if len(token) < 32 {
		return nil, errors.New("API token must contain at least 32 characters")
	}
	// State and API credentials must never be included in a permitted backup root.
	for _, root := range c.SourceRoots {
		realRoot, rootErr := canonicalPath(root)
		if rootErr != nil {
			return nil, errors.New("invalid source root")
		}
		for _, protected := range []string{c.StateDir, c.TokenFile} {
			realProtected, privateErr := canonicalPath(protected)
			if privateErr != nil || inside(realRoot, realProtected) || inside(realProtected, realRoot) {
				return nil, errors.New("source roots must exclude state directory and API token")
			}
		}
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, err
	}
	s := &Server{cfg: c, token: token, engine: e, ctx: ctx, state: State{Jobs: []Job{}, Runs: []Run{}}}
	b, err := os.ReadFile(filepath.Join(c.StateDir, "state.json"))
	if err == nil {
		if err = json.Unmarshal(b, &s.state); err != nil {
			return nil, fmt.Errorf("invalid persisted state: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	for i := range s.state.Runs {
		if s.state.Runs[i].Status == "running" {
			now := time.Now().UTC()
			s.state.Runs[i].Status = "failed"
			s.state.Runs[i].Error = "service interrupted; verify repository before retrying"
			s.state.Runs[i].Finished = &now
		}
	}
	if err = s.save(); err != nil {
		return nil, err
	}
	return s, nil
}
func canonicalPath(path string) (string, error) {
	path = filepath.Clean(path)
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	real, err = canonicalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(real, filepath.Base(path)), nil
}
func inside(root, path string) bool {
	r, e := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// save is called under mu, or before the server is exposed.
func (s *Server) save() error {
	b, e := json.MarshalIndent(s.state, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(s.cfg.StateDir, ".state-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(name, filepath.Join(s.cfg.StateDir, "state.json"))
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	jsonOut(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fail(w, 400, "invalid JSON body")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		fail(w, 400, "expected one JSON object")
		return false
	}
	return true
}
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, map[string]string{"status": "ok"}) })
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
	if strings.HasPrefix(r.URL.Path, "/api/cbt/") {
		s.cbtAPI(w, r)
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/status":
		s.mu.Lock()
		defer s.mu.Unlock()
		jsonOut(w, 200, map[string]any{"version": version, "busy": s.busy, "source_roots": s.cfg.SourceRoots, "restore_root": s.cfg.RestoreRoot})
	case r.Method == "GET" && r.URL.Path == "/api/jobs":
		s.mu.Lock()
		defer s.mu.Unlock()
		jsonOut(w, 200, s.state.Jobs)
	case r.Method == "GET" && r.URL.Path == "/api/runs":
		s.mu.Lock()
		defer s.mu.Unlock()
		jsonOut(w, 200, s.state.Runs)
	case (r.Method == "PATCH" || r.Method == "DELETE") && strings.HasPrefix(r.URL.Path, "/api/jobs/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if r.Method == "PATCH" {
			if !decode(w, r, &body) {
				return
			}
			if body.Enabled == nil {
				fail(w, 400, "enabled is required")
				return
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, j := range s.state.Jobs {
			if j.ID == id {
				old := append([]Job(nil), s.state.Jobs...)
				if r.Method == "DELETE" {
					s.state.Jobs = append(s.state.Jobs[:i], s.state.Jobs[i+1:]...)
				} else {
					s.state.Jobs[i].Enabled = *body.Enabled
				}
				if e := s.save(); e != nil {
					s.state.Jobs = old
					fail(w, 500, "cannot persist job change")
					return
				}
				if r.Method == "DELETE" {
					w.WriteHeader(204)
				} else {
					jsonOut(w, 200, s.state.Jobs[i])
				}
				return
			}
		}
		fail(w, 404, "job not found")
	case r.Method == "POST" && r.URL.Path == "/api/jobs":
		var j Job
		if !decode(w, r, &j) {
			return
		}
		j.Name = strings.TrimSpace(j.Name)
		if j.Name == "" || len(j.Name) > 100 || j.Interval < 1 || j.Interval > 525600 || len(j.Paths) > 64 {
			fail(w, 400, "name, 1..525600 interval_minutes and 1..64 source paths required")
			return
		}
		if e := s.engine.ValidateSources(j.Paths); e != nil {
			fail(w, 400, e.Error())
			return
		}
		j.ID = uid()
		j.LastAttempt = time.Time{}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.state.Jobs) >= 100 {
			fail(w, 400, "maximum 100 jobs")
			return
		}
		s.state.Jobs = append(s.state.Jobs, j)
		if e := s.save(); e != nil {
			s.state.Jobs = s.state.Jobs[:len(s.state.Jobs)-1]
			fail(w, 500, "cannot persist job")
			return
		}
		jsonOut(w, 201, j)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/jobs/") && strings.HasSuffix(r.URL.Path, "/run"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/jobs/"), "/run")
		s.startHTTP(w, "backup", id, "", "")
	case r.Method == "POST" && r.URL.Path == "/api/init":
		s.startHTTP(w, "init", "", "", "")
	case r.Method == "POST" && r.URL.Path == "/api/check":
		s.startHTTP(w, "check", "", "", "")
	case r.Method == "POST" && r.URL.Path == "/api/restore":
		var body struct {
			Snapshot string `json:"snapshot"`
			Target   string `json:"target_name"`
		}
		if !decode(w, r, &body) {
			return
		}
		s.startHTTP(w, "restore", "", body.Snapshot, body.Target)
	case r.Method == "GET" && r.URL.Path == "/api/snapshots":
		s.mu.Lock()
		if s.busy {
			s.mu.Unlock()
			fail(w, 409, "repository busy")
			return
		}
		s.busy = true
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.busy = false; s.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		data, e := s.engine.Snapshots(ctx)
		if e != nil {
			fail(w, 500, e.Error())
			return
		}
		jsonOut(w, 200, data)
	default:
		fail(w, 404, "API route not found")
	}
}
func (s *Server) startHTTP(w http.ResponseWriter, kind, job, snapshot, target string) {
	id, status, e := s.start(kind, job, snapshot, target)
	if e != nil {
		fail(w, status, e.Error())
		return
	}
	jsonOut(w, 202, map[string]string{"id": id})
}
func (s *Server) start(kind, jobID, snapshot, target string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return "", 503, errors.New("service stopping")
	}
	if s.busy {
		return "", 409, errors.New("repository busy")
	}
	var job Job
	index := -1
	if kind == "backup" {
		for i, j := range s.state.Jobs {
			if j.ID == jobID {
				job = j
				index = i
				break
			}
		}
		if index < 0 {
			return "", 404, errors.New("job not found")
		}
	}
	previous := s.state
	previous.Runs = append([]Run(nil), s.state.Runs...)
	previous.Jobs = append([]Job(nil), s.state.Jobs...)
	run := Run{ID: uid(), JobID: jobID, Kind: kind, Status: "running", Started: time.Now().UTC()}
	if index >= 0 {
		s.state.Jobs[index].LastAttempt = run.Started
	}
	s.state.Runs = append([]Run{run}, s.state.Runs...)
	if len(s.state.Runs) > 1000 {
		s.state.Runs = s.state.Runs[:1000]
	}
	if e := s.save(); e != nil {
		s.state = previous
		return "", 500, errors.New("cannot persist operation")
	}
	s.busy = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(s.ctx, time.Duration(s.cfg.Timeout)*time.Minute)
		defer cancel()
		var err error
		var restored string
		switch kind {
		case "init":
			err = s.engine.Init(ctx)
		case "check":
			err = s.engine.Check(ctx)
		case "backup":
			err = s.engine.Backup(ctx, job.Paths, job.ID)
		case "restore":
			restored, err = s.engine.Restore(ctx, snapshot, target)
		default:
			err = errors.New("unsupported operation")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		now := time.Now().UTC()
		for i := range s.state.Runs {
			if s.state.Runs[i].ID == run.ID {
				r := &s.state.Runs[i]
				r.Finished = &now
				r.Target = restored
				r.Status = "succeeded"
				if err != nil {
					r.Status = "failed"
					r.Error = err.Error()
				}
				break
			}
		}
		if e := s.save(); e != nil {
			log.Print("cannot persist completed operation; inspect state directory")
		}
		s.busy = false
	}()
	return run.ID, 202, nil
}
func (s *Server) schedule(now time.Time) {
	s.mu.Lock()
	jobs := append([]Job(nil), s.state.Jobs...)
	s.mu.Unlock()
	for _, j := range jobs {
		if j.Enabled && (j.LastAttempt.IsZero() || now.Sub(j.LastAttempt) >= time.Duration(j.Interval)*time.Minute) {
			_, _, _ = s.start("backup", j.ID, "", "")
		}
	}
}
func main() {
	path := flag.String("config", "config.json", "configuration file")
	flag.Parse()
	cfg, err := loadConfig(*path)
	if err != nil {
		log.Fatal(err)
	}
	e, err := engine.New(engine.Config{Binary: cfg.ResticBinary, Repository: cfg.Repository, PasswordFile: cfg.PasswordFile, RestoreRoot: cfg.RestoreRoot, SourceRoots: cfg.SourceRoots})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := newServer(ctx, cfg, e)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				s.schedule(now)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("Open Vault %s listening at http://%s", version, cfg.Listen)
	if err = srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	stop()
	s.wg.Wait()
}
