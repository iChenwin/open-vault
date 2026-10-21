// Package engine runs a narrowly scoped restic CLI for local Linux file backups.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

type Config struct {
	Binary, Repository, PasswordFile, RestoreRoot string
	SourceRoots                                   []string
}

type Engine struct{ config Config }

// New validates configuration and resolves symlinks before recording trust roots.
func New(c Config) (*Engine, error) {
	if c.Binary == "" {
		c.Binary = "restic"
	}
	if !filepath.IsAbs(c.Repository) || !filepath.IsAbs(c.PasswordFile) || !filepath.IsAbs(c.RestoreRoot) {
		return nil, errors.New("repository, password file and restore root must be absolute paths")
	}
	var err error
	if c.Repository, err = canonical(c.Repository); err != nil {
		return nil, errors.New("invalid repository path")
	}
	if c.PasswordFile, err = filepath.EvalSymlinks(c.PasswordFile); err != nil {
		return nil, errors.New("password file is unavailable")
	}
	info, err := os.Stat(c.PasswordFile)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, errors.New("password file must be a nonempty regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("password file must have private permissions (0600)")
	}
	if err := os.MkdirAll(c.RestoreRoot, 0700); err != nil {
		return nil, errors.New("cannot prepare restore root")
	}
	if c.RestoreRoot, err = filepath.EvalSymlinks(c.RestoreRoot); err != nil {
		return nil, errors.New("invalid restore root")
	}
	if len(c.SourceRoots) == 0 {
		return nil, errors.New("at least one source root is required")
	}
	roots := make([]string, 0, len(c.SourceRoots))
	for _, root := range c.SourceRoots {
		if !filepath.IsAbs(root) {
			return nil, errors.New("source roots must be absolute directories")
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, errors.New("source root is unavailable")
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return nil, errors.New("source roots must be directories")
		}
		roots = append(roots, filepath.Clean(resolved))
	}
	c.SourceRoots = roots
	for _, protected := range append(append([]string{}, roots...), c.Repository, c.PasswordFile) {
		if within(c.RestoreRoot, protected) || within(protected, c.RestoreRoot) {
			return nil, errors.New("restore root must not overlap sources, repository or credentials")
		}
	}
	e := &Engine{config: c}
	return e, nil
}

// canonical resolves an existing ancestor when a repository has not yet been initialized.
func canonical(path string) (string, error) {
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = canonical(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (e *Engine) sources(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("at least one source is required")
	}
	resolvedPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, errors.New("sources must be absolute paths")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, errors.New("source is unavailable")
		}
		allowed := false
		for _, root := range e.config.SourceRoots {
			if within(root, resolved) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, errors.New("source is outside approved roots")
		}
		// Refuse both ancestors and descendants of private backup data.
		for _, private := range []string{e.config.Repository, e.config.RestoreRoot, e.config.PasswordFile} {
			current, err := canonical(private)
			if err != nil || current != private {
				return nil, errors.New("private backup paths changed; restart required")
			}
			if within(resolved, private) || within(private, resolved) {
				return nil, errors.New("source overlaps repository, restore area or credentials")
			}
		}
		resolvedPaths = append(resolvedPaths, resolved)
	}
	return resolvedPaths, nil
}

func (e *Engine) ValidateSources(paths []string) error { _, err := e.sources(paths); return err }

// cappedBuffer drains output without allowing a subprocess to exhaust server memory.
type cappedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (4 << 20) - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func (e *Engine) run(ctx context.Context, args ...string) ([]byte, error) {
	base := []string{"--repo", e.config.Repository, "--password-file", e.config.PasswordFile}
	cmd := exec.CommandContext(ctx, e.config.Binary, append(base, args...)...)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "RESTIC_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	var output cappedBuffer
	cmd.Stdout = &output
	// restic diagnostics can include secret paths. Return only controlled errors.
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("restic operation failed; check repository, credentials and restic installation")
	}
	if output.overflow {
		return nil, errors.New("restic response exceeded size limit")
	}
	return output.Bytes(), nil
}

func (e *Engine) Init(ctx context.Context) error { _, err := e.run(ctx, "init"); return err }
func (e *Engine) Check(ctx context.Context) error {
	_, err := e.run(ctx, "check", "--read-data")
	return err
}
func (e *Engine) Backup(ctx context.Context, paths []string, tag string) error {
	resolved, err := e.sources(paths)
	if err != nil {
		return err
	}
	if len(tag) > 128 || strings.ContainsAny(tag, "\x00\r\n") {
		return errors.New("invalid backup tag")
	}
	args := []string{"backup", "--quiet"}
	if tag != "" {
		args = append(args, "--tag="+tag)
	}
	args = append(args, "--")
	_, err = e.run(ctx, append(args, resolved...)...)
	return err
}
func (e *Engine) Snapshots(ctx context.Context) (json.RawMessage, error) {
	output, err := e.run(ctx, "snapshots", "--json")
	if err != nil {
		return nil, err
	}
	if !json.Valid(output) {
		return nil, errors.New("restic returned invalid snapshot data")
	}
	return json.RawMessage(output), nil
}

var snapshotPattern = regexp.MustCompile(`^[a-fA-F0-9]{8,64}$`)
var targetPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

func (e *Engine) Restore(ctx context.Context, snapshot, targetName string) (string, error) {
	if !snapshotPattern.MatchString(snapshot) {
		return "", errors.New("snapshot must be an 8–64 digit hexadecimal ID")
	}
	if !targetPattern.MatchString(targetName) {
		return "", errors.New("restore name must use letters, numbers, underscores or hyphens")
	}
	root, err := filepath.EvalSymlinks(e.config.RestoreRoot)
	if err != nil || root != e.config.RestoreRoot {
		return "", errors.New("restore root changed; restart required")
	}
	target := filepath.Join(root, targetName)
	if err := os.Mkdir(target, 0700); err != nil {
		return "", errors.New("restore destination must be a new directory")
	}
	if _, err := e.run(ctx, "restore", snapshot, "--target", target); err != nil {
		return "", fmt.Errorf("restore failed (partial directory retained): %w", err)
	}
	return target, nil
}
