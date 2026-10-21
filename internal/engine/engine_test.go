package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	password := filepath.Join(base, "credentials-secret")
	if err := os.WriteFile(password, []byte("a-test-only-password"), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{Repository: filepath.Join(base, "repo"), PasswordFile: password, RestoreRoot: filepath.Join(base, "restored"), SourceRoots: []string{source}}
}

func TestSourceBoundaries(t *testing.T) {
	c := testConfig(t)
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(c.SourceRoots[0], "hello")
	if err := os.WriteFile(file, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.ValidateSources([]string{file}); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{nil, {"relative"}, {filepath.Dir(c.SourceRoots[0])}, {c.PasswordFile}, {c.RestoreRoot}, {file + "-missing"}} {
		if err := e.ValidateSources(paths); err == nil {
			t.Errorf("accepted invalid source %v", paths)
		}
	}
	// Even an approved source root must not contain credentials or repository data.
	c.SourceRoots = []string{filepath.Dir(c.PasswordFile)}
	if _, err = New(c); err == nil {
		t.Fatal("accepted source root containing restore area")
	}
}

func TestRestoreConfigOverlap(t *testing.T) {
	for _, destination := range []string{"source", "repo", "password", "parent"} {
		t.Run(destination, func(t *testing.T) {
			c := testConfig(t)
			switch destination {
			case "source":
				c.RestoreRoot = filepath.Join(c.SourceRoots[0], "recovered")
			case "repo":
				c.RestoreRoot = c.Repository
			case "password":
				c.RestoreRoot = filepath.Dir(c.PasswordFile)
			case "parent":
				c.RestoreRoot = filepath.Dir(c.SourceRoots[0])
			}
			if _, err := New(c); err == nil {
				t.Fatal("accepted overlapping restore configuration")
			}
		})
	}
}

func TestPasswordPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode enforcement")
	}
	c := testConfig(t)
	if err := os.Chmod(c.PasswordFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(c); err == nil {
		t.Fatal("accepted public password file")
	}
}

func TestSymlinkEscape(t *testing.T) {
	c := testConfig(t)
	outside := t.TempDir()
	link := filepath.Join(c.SourceRoots[0], "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable on this host")
	}
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ValidateSources([]string{link}); err == nil {
		t.Fatal("accepted source symlink outside allowlist")
	}
}

func TestRestoreBoundaries(t *testing.T) {
	c := testConfig(t)
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"..", "../escape", "a/b", "a\\b", "", "x\x00x"} {
		if _, err := e.Restore(context.Background(), "12345678", name); err == nil {
			t.Errorf("accepted invalid target %q", name)
		}
	}
	for _, id := range []string{"latest", "abcd", "--help", "1234567z", strings.Repeat("a", 65)} {
		if _, err := e.Restore(context.Background(), id, "new"); err == nil {
			t.Errorf("accepted invalid snapshot %q", id)
		}
	}
	if err := os.Mkdir(filepath.Join(c.RestoreRoot, "existing"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Restore(context.Background(), "12345678", "existing"); err == nil {
		t.Fatal("accepted existing restore target")
	}
}

func TestConfigurationAndErrors(t *testing.T) {
	c := testConfig(t)
	c.Repository = "relative"
	if _, err := New(c); err == nil {
		t.Fatal("accepted relative repository")
	}
	c = testConfig(t)
	c.Binary = filepath.Join(t.TempDir(), "nonexistent-restic")
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	err = e.Init(context.Background())
	if err == nil {
		t.Fatal("expected unavailable binary error")
	}
	if strings.Contains(err.Error(), c.PasswordFile) || strings.Contains(err.Error(), "a-test-only-password") {
		t.Fatal("error exposed credentials")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Check(ctx); err != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestOutputLimit(t *testing.T) {
	var b cappedBuffer
	payload := make([]byte, 5<<20)
	n, err := b.Write(payload)
	if err != nil || n != len(payload) || !b.overflow || b.Len() != 4<<20 {
		t.Fatal("output limit failed")
	}
	if _, err := b.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
}

// Set RESTIC_TEST_BINARY to an installed restic executable to run a real encrypted
// backup, repository verification and restore round trip on Linux or Windows.
func TestResticRoundTrip(t *testing.T) {
	binary := os.Getenv("RESTIC_TEST_BINARY")
	if binary == "" {
		t.Skip("RESTIC_TEST_BINARY is not configured")
	}
	c := testConfig(t)
	c.Binary = binary
	e, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	payload := []byte("restore this original content\n")
	source := filepath.Join(c.SourceRoots[0], "payload.txt")
	if err := os.WriteFile(source, payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Backup(ctx, c.SourceRoots, "round-trip"); err != nil {
		t.Fatal(err)
	}
	data, err := e.Snapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &snapshots); err != nil || len(snapshots) != 1 {
		t.Fatalf("invalid snapshots: %s (%v)", data, err)
	}
	originalID := snapshots[0].ID
	if err := os.WriteFile(source, []byte("second version with a different length\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.Backup(ctx, c.SourceRoots, "round-trip"); err != nil {
		t.Fatal(err)
	}
	data, err = e.Snapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &snapshots); err != nil || len(snapshots) != 2 {
		t.Fatalf("expected two snapshot versions: %s (%v)", data, err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := e.Check(ctx); err != nil {
		t.Fatal(err)
	}
	target, err := e.Restore(ctx, originalID, "recovery-1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	err = filepath.WalkDir(target, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && d.Name() == "payload.txt" {
			got, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if string(got) != string(payload) {
				t.Errorf("restored content mismatch: %q", got)
			}
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("restored file missing")
	}
	if _, err := e.Restore(ctx, originalID, "recovery-1"); err == nil {
		t.Fatal("restore overwrote existing destination")
	}
}
