// Package cbtservice exposes a fixed CBT workflow over a local Unix socket.
package cbtservice

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ichenwin/open-vault/internal/blockstore"
)

type Config struct {
	SocketPath              string `json:"socket_path"`
	CLIBinary               string `json:"cli_binary"`
	CLIConfig               string `json:"cli_config"`
	StateDir                string `json:"state_dir"`
	RestoreRoot             string `json:"restore_root"`
	OperationTimeoutMinutes int    `json:"operation_timeout_minutes"`
}

// ServiceConfig is an alias for callers that prefer an explicit type name.
type ServiceConfig = Config

type Run struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	Status          string     `json:"status"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Error           string     `json:"error,omitempty"`
	GenerationID    string     `json:"generation_id,omitempty"`
	Target          string     `json:"target,omitempty"`
	SourceReadBytes int64      `json:"source_read_bytes"`
}

type Service struct {
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	cfg               Config
	runs              []Run
	busy              bool
	serving           bool
	persistenceFailed bool
	wg                sync.WaitGroup
	timeout           time.Duration
	execute           func(context.Context, ...string) ([]byte, []byte, error)
}

var generationName = regexp.MustCompile(`^[a-f0-9]{32}$`)
var restoreName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func privateDirectory(path string, allowGroup bool) (string, error) {
	if err := os.MkdirAll(path, 0700); err != nil {
		return "", errors.New("cannot prepare agent directory")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("cannot resolve agent directory")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", errors.New("agent directory is unavailable")
	}
	if err := secureOwner(info); err != nil {
		return "", err
	}
	if err := secureAncestors(real); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		mode := info.Mode().Perm()
		if mode != 0700 && (!allowGroup || mode != 0750) {
			return "", errors.New("agent directories require 0700, or 0750 for socket traversal")
		}
	}
	return real, nil
}

// LoadConfig reads only a fixed, private startup configuration. No request can
// replace the CLI executable, CLI configuration or approved restore directory.
func LoadConfig(path string) (Config, error) {
	var c Config
	if !filepath.IsAbs(path) {
		return c, errors.New("agent config path must be absolute")
	}
	f, err := os.Open(path)
	if err != nil {
		return c, errors.New("cannot open agent configuration")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return c, errors.New("agent configuration must be a private regular file (0600)")
	}
	if err := secureOwner(info); err != nil {
		return c, err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return c, errors.New("cannot resolve agent configuration")
	}
	if err := secureAncestors(canonical); err != nil {
		return c, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return c, errors.New("agent configuration is too large")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil || d.Decode(new(any)) != io.EOF {
		return c, errors.New("invalid agent configuration")
	}
	return c, nil
}

func New(ctx context.Context, c Config) (*Service, error) {
	for _, p := range []string{c.SocketPath, c.CLIBinary, c.CLIConfig, c.StateDir, c.RestoreRoot} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\r\n") {
			return nil, errors.New("agent configuration paths must be absolute")
		}
	}
	if c.OperationTimeoutMinutes == 0 {
		c.OperationTimeoutMinutes = 120
	}
	if c.OperationTimeoutMinutes < 1 || c.OperationTimeoutMinutes > 10080 {
		return nil, errors.New("operation timeout must be 1..10080 minutes")
	}
	var err error
	if c.CLIBinary, err = filepath.EvalSymlinks(c.CLIBinary); err != nil {
		return nil, errors.New("configured CBT CLI is unavailable")
	}
	binaryInfo, err := os.Stat(c.CLIBinary)
	if err != nil || !binaryInfo.Mode().IsRegular() || (runtime.GOOS != "windows" && (binaryInfo.Mode().Perm()&0022 != 0 || binaryInfo.Mode().Perm()&0100 == 0)) {
		return nil, errors.New("CBT CLI must be an executable file without group or world write access")
	}
	if err := secureOwner(binaryInfo); err != nil {
		return nil, err
	}
	if err := secureAncestors(c.CLIBinary); err != nil {
		return nil, err
	}
	if c.CLIConfig, err = filepath.EvalSymlinks(c.CLIConfig); err != nil {
		return nil, errors.New("configured CBT CLI config is unavailable")
	}
	configInfo, err := os.Stat(c.CLIConfig)
	if err != nil || !configInfo.Mode().IsRegular() || (runtime.GOOS != "windows" && configInfo.Mode().Perm()&0077 != 0) {
		return nil, errors.New("CBT CLI config must be a private regular file (0600)")
	}
	if err := secureOwner(configInfo); err != nil {
		return nil, err
	}
	if err := secureAncestors(c.CLIConfig); err != nil {
		return nil, err
	}
	if c.StateDir, err = privateDirectory(c.StateDir, false); err != nil {
		return nil, err
	}
	if c.RestoreRoot, err = privateDirectory(c.RestoreRoot, false); err != nil {
		return nil, err
	}
	for _, forbidden := range []string{"/dev", "/proc", "/sys"} {
		if runtime.GOOS == "linux" && (within(forbidden, c.RestoreRoot) || within(c.RestoreRoot, forbidden)) {
			return nil, errors.New("restore root cannot overlap system device or kernel filesystems")
		}
	}
	if err := protectCLIPaths(c); err != nil {
		return nil, err
	}
	parent, err := privateDirectory(filepath.Dir(c.SocketPath), true)
	if err != nil {
		return nil, err
	}
	c.SocketPath = filepath.Join(parent, filepath.Base(c.SocketPath))
	if len(c.SocketPath) > 100 && runtime.GOOS == "linux" {
		return nil, errors.New("Unix socket path exceeds safe length")
	}
	for _, p := range []string{c.StateDir, parent, c.CLIBinary, c.CLIConfig} {
		if within(c.RestoreRoot, p) || within(p, c.RestoreRoot) {
			return nil, errors.New("restore root must be separate from agent state, socket and CLI configuration")
		}
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	s := &Service{ctx: serviceCtx, cancel: cancel, cfg: c, runs: []Run{}, timeout: time.Duration(c.OperationTimeoutMinutes) * time.Minute}
	s.execute = s.runCLI
	if err := s.load(); err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

func protectCLIPaths(c Config) error {
	f, err := os.Open(c.CLIConfig)
	if err != nil {
		return errors.New("cannot inspect fixed CLI configuration")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return errors.New("CLI configuration exceeds size limit")
	}
	var paths struct {
		Repository string `json:"repository"`
		KeyFile    string `json:"key_file"`
		StateDir   string `json:"state_dir"`
		Volume     struct {
			MountPoint string `json:"mount_point"`
		} `json:"volume"`
	}
	if err := json.Unmarshal(b, &paths); err != nil {
		return errors.New("invalid fixed CLI configuration")
	}
	for _, p := range []string{paths.Repository, paths.KeyFile, paths.StateDir, paths.Volume.MountPoint} {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			return errors.New("CLI protected paths must be absolute")
		}
		real, err := resolvePotential(p)
		if err != nil {
			return errors.New("cannot resolve CLI protected path")
		}
		if within(c.RestoreRoot, real) || within(real, c.RestoreRoot) {
			return errors.New("restore root overlaps CLI source, repository, state or credentials")
		}
	}
	return nil
}

func resolvePotential(path string) (string, error) {
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
	real, err = resolvePotential(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(real, filepath.Base(path)), nil
}

func (s *Service) statePath() string { return filepath.Join(s.cfg.StateDir, "runs.json") }

func (s *Service) load() error {
	path := s.statePath()
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return s.save()
	}
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("agent run history must be a private regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("cannot open agent run history")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	closeErr := f.Close()
	if err != nil || len(b) > 8<<20 {
		return errors.New("agent run history exceeds size limit")
	}
	if closeErr != nil {
		return errors.New("cannot close agent run history")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&s.runs); err != nil || d.Decode(new(any)) != io.EOF || s.runs == nil || len(s.runs) > 1000 {
		return errors.New("invalid agent run history")
	}
	for i := range s.runs {
		if s.runs[i].Status == "running" {
			now := time.Now().UTC()
			s.runs[i].FinishedAt = &now
			s.runs[i].Status = "failed"
			s.runs[i].Error = "agent interrupted; inspect driver and repository before retrying"
		}
	}
	return s.save()
}

func (s *Service) save() error {
	b, err := json.MarshalIndent(s.runs, "", "  ")
	if err != nil {
		return errors.New("cannot encode agent run history")
	}
	f, err := os.CreateTemp(s.cfg.StateDir, ".runs-*")
	if err != nil {
		return errors.New("cannot prepare agent run history")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return errors.New("cannot persist agent run history")
	}
	if err = os.Rename(name, s.statePath()); err != nil {
		return errors.New("cannot commit agent run history")
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(s.cfg.StateDir)
		if err != nil {
			return errors.New("cannot open agent state directory")
		}
		defer dir.Close()
		if err = dir.Sync(); err != nil {
			return errors.New("cannot sync agent state directory")
		}
	}
	return nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func (s *Service) runCLI(ctx context.Context, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, s.cfg.CLIBinary, args...)
	configureProcess(cmd)
	var stdout, stderr limitedBuffer
	stdout.limit = 4 << 20
	stderr.limit = 64 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Laboratory hooks cannot be enabled by the web process or inherited from
	// a deployment shell. Fixed startup configuration remains the only input.
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "OPEN_VAULT_CBT_LAB") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if stdout.overflow || stderr.overflow {
		return nil, nil, errors.New("CBT CLI output exceeded safety limit")
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

type cliResult struct {
	Generation             blockstore.Generation `json:"generation"`
	ThisRunSourceReadBytes *int64                `json:"this_run_source_read_bytes"`
	Committed              bool                  `json:"committed"`
}

func decodeResult(data []byte) (cliResult, error) {
	var out cliResult
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&out); err != nil || d.Decode(new(any)) != io.EOF {
		return out, errors.New("CBT CLI returned invalid JSON")
	}
	if !generationName.MatchString(out.Generation.ID) || out.Generation.Size <= 0 || out.Generation.SourceReadBytes < 0 || out.Generation.SourceReadBytes > out.Generation.Size {
		return out, errors.New("CBT CLI returned invalid generation data")
	}
	if out.ThisRunSourceReadBytes != nil && (*out.ThisRunSourceReadBytes < 0 || *out.ThisRunSourceReadBytes > out.Generation.Size) {
		return out, errors.New("CBT CLI returned invalid source byte count")
	}
	return out, nil
}

func controlledError(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "CBT operation timed out; inspect driver and repository before retrying"
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "CBT operation interrupted; inspect driver and repository before retrying"
	}
	return "CBT CLI operation failed; inspect agent, driver and repository"
}

func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	jsonOut(w, status, map[string]string{"error": message})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	err := d.Decode(v)
	if err == io.EOF && optional {
		return true
	}
	if err != nil || d.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid request body")
		return false
	}
	return true
}

func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/status":
			s.mu.Lock()
			busy := s.busy
			s.mu.Unlock()
			jsonOut(w, 200, map[string]any{"busy": busy, "configured": true})
		case r.Method == "GET" && r.URL.Path == "/runs":
			s.mu.Lock()
			defer s.mu.Unlock()
			jsonOut(w, 200, s.runs)
		case r.Method == "GET" && r.URL.Path == "/head":
			s.head(w, r)
		case r.Method == "POST" && r.URL.Path == "/backup":
			var body struct{}
			if !decodeBody(w, r, &body, true) {
				return
			}
			s.start(w, "backup", "", "")
		case r.Method == "POST" && r.URL.Path == "/verify":
			var body struct {
				Generation string `json:"generation"`
			}
			if !decodeBody(w, r, &body, true) {
				return
			}
			if body.Generation != "" && !generationName.MatchString(body.Generation) {
				fail(w, 400, "generation must be a 32 digit lowercase hexadecimal ID")
				return
			}
			s.start(w, "verify", body.Generation, "")
		case r.Method == "POST" && r.URL.Path == "/restore":
			var body struct {
				Generation string `json:"generation"`
				TargetName string `json:"target_name"`
			}
			if !decodeBody(w, r, &body, false) {
				return
			}
			if !generationName.MatchString(body.Generation) || !restoreName.MatchString(body.TargetName) {
				fail(w, 400, "valid generation and safe target_name are required")
				return
			}
			s.start(w, "restore", body.Generation, body.TargetName)
		default:
			fail(w, 404, "agent route not found")
		}
	})
}

func (s *Service) head(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		fail(w, 409, "repository busy")
		return
	}
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		fail(w, 503, "agent stopping")
		return
	}
	s.busy = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.busy = false; s.mu.Unlock(); s.wg.Done() }()
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	stdout, stderr, err := s.execute(ctx, "-config", s.cfg.CLIConfig, "status")
	if err != nil {
		var diagnostic struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(stderr, &diagnostic) == nil && diagnostic.Error == "repository has no generations" {
			jsonOut(w, 200, map[string]any{"generation": nil})
			return
		}
		fail(w, 502, controlledError(ctx, err))
		return
	}
	out, err := decodeResult(stdout)
	if err != nil {
		fail(w, 502, "CBT CLI returned invalid generation data")
		return
	}
	jsonOut(w, 200, map[string]any{"generation": out.Generation})
}

func (s *Service) start(w http.ResponseWriter, kind, generation, targetName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		fail(w, 503, "agent stopping")
		return
	}
	if s.persistenceFailed {
		fail(w, 503, "agent history requires administrator inspection")
		return
	}
	if s.busy {
		fail(w, 409, "repository busy")
		return
	}
	target := ""
	if kind == "restore" {
		root, err := filepath.EvalSymlinks(s.cfg.RestoreRoot)
		if err != nil || root != s.cfg.RestoreRoot {
			fail(w, 503, "approved restore directory changed")
			return
		}
		target = filepath.Join(root, targetName+".img")
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			fail(w, 409, "restore target must be a new image file")
			return
		}
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		fail(w, 500, "cannot allocate run ID")
		return
	}
	run := Run{ID: hex.EncodeToString(idBytes[:]), Kind: kind, Status: "running", StartedAt: time.Now().UTC(), GenerationID: generation, Target: target}
	previous := s.runs
	s.runs = append([]Run{run}, s.runs...)
	if len(s.runs) > 1000 {
		s.runs = s.runs[:1000]
	}
	if err := s.save(); err != nil {
		s.runs = previous
		s.persistenceFailed = true
		fail(w, 500, "cannot persist operation")
		return
	}
	s.busy = true
	s.wg.Add(1)
	go s.perform(run)
	jsonOut(w, 202, map[string]string{"id": run.ID})
}

func (s *Service) perform(run Run) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
	defer cancel()
	args := []string{"-config", s.cfg.CLIConfig}
	if run.GenerationID != "" {
		args = append(args, "-generation", run.GenerationID)
	}
	if run.Target != "" {
		args = append(args, "-target", run.Target)
	}
	args = append(args, run.Kind)
	stdout, stderr, commandErr := s.execute(ctx, args...)
	message := ""
	var out cliResult
	if commandErr != nil {
		message = controlledError(ctx, commandErr)
		// Only selected validated metadata survives a published-but-failed CLI
		// response. Raw diagnostics may contain credential paths and are discarded.
		if candidate, err := decodeResult(stderr); err == nil && candidate.Committed {
			out = candidate
			message = "image published but operation failed; inspect driver and repository before retrying"
		}
	} else {
		var err error
		out, err = decodeResult(stdout)
		if err != nil {
			message = "CBT CLI returned invalid generation data; inspect repository"
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.runs {
		if s.runs[i].ID == run.ID {
			r := &s.runs[i]
			now := time.Now().UTC()
			r.FinishedAt = &now
			r.Status = "succeeded"
			if message != "" {
				r.Status = "failed"
				r.Error = message
			}
			if out.Generation.ID != "" {
				r.GenerationID = out.Generation.ID
			}
			if run.Kind == "backup" && out.ThisRunSourceReadBytes != nil {
				r.SourceReadBytes = *out.ThisRunSourceReadBytes
			}
			break
		}
	}
	if err := s.save(); err != nil {
		s.persistenceFailed = true
	}
	s.busy = false
}

// Serve creates a new Unix socket and never unlinks a pre-existing path. The
// process effective group owns the socket; deploy with Group=open-vault.
func (s *Service) Serve(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return errors.New("CBT agent socket service requires Linux")
	}
	s.mu.Lock()
	if s.serving {
		s.mu.Unlock()
		return errors.New("CBT agent is already serving")
	}
	s.serving = true
	s.mu.Unlock()
	if _, err := os.Lstat(s.cfg.SocketPath); !os.IsNotExist(err) {
		return errors.New("agent socket path already exists; inspect stale socket manually")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.cfg.SocketPath, Net: "unix"})
	if err != nil {
		return errors.New("cannot bind agent Unix socket")
	}
	listener.SetUnlinkOnClose(false)
	owned, err := os.Lstat(s.cfg.SocketPath)
	if err != nil {
		listener.Close()
		return errors.New("cannot inspect agent Unix socket")
	}
	defer func() {
		listener.Close()
		current, err := os.Lstat(s.cfg.SocketPath)
		if err == nil && os.SameFile(owned, current) {
			_ = os.Remove(s.cfg.SocketPath)
		}
	}()
	if err := os.Chmod(s.cfg.SocketPath, 0660); err != nil {
		return errors.New("cannot restrict agent Unix socket")
	}
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-s.ctx.Done():
		case <-done:
			return
		}
		s.cancel()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.Serve(listener)
	close(done)
	s.cancel()
	s.wg.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("agent Unix socket server stopped unexpectedly")
	}
	return nil
}
