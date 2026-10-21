// open-vault-cbt is an experimental, administrator-only data-volume backup CLI.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ichenwin/open-vault/internal/blockstore"
	"github.com/ichenwin/open-vault/internal/cbt"
)

type config struct {
	Repository string     `json:"repository"`
	KeyFile    string     `json:"key_file"`
	StateDir   string     `json:"state_dir"`
	Volume     cbt.Volume `json:"volume"`
	DBDCtl     string     `json:"dbdctl"`
}
type tracker interface {
	SourceSize(context.Context, string) (int64, error)
	Current(context.Context, string) (cbt.Transition, error)
	Setup(context.Context, string, string) (cbt.Generation, error)
	ToSnapshot(context.Context, string, string) (cbt.Transition, error)
	ToIncremental(context.Context, string, cbt.Generation) error
	Verify(context.Context, string, cbt.Generation) error
}
type backupResult struct {
	Generation             blockstore.Generation `json:"generation"`
	TrackingIndexReadBytes int64                 `json:"tracking_index_read_bytes"`
	OracleReadBytes        int64                 `json:"oracle_read_bytes,omitempty"`
	FinalizedExisting      bool                  `json:"finalized_existing,omitempty"`
	ThisRunSourceReadBytes int64                 `json:"this_run_source_read_bytes"`
}
type publishedError struct {
	Generation blockstore.Generation
	Durability string
	Cause      error
}

func (e *publishedError) Error() string {
	return fmt.Sprintf("generation %s published; durability=%s; snapshot retained for inspection: %v", e.Generation.ID, e.Durability, e.Cause)
}
func (e *publishedError) Unwrap() error { return e.Cause }

func randomName() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func resolve(path string) (string, error) {
	path = filepath.Clean(path)
	real, e := filepath.EvalSymlinks(path)
	if e == nil {
		return real, nil
	}
	if !os.IsNotExist(e) {
		return "", e
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", e
	}
	real, e = resolve(parent)
	if e != nil {
		return "", e
	}
	return filepath.Join(real, filepath.Base(path)), nil
}
func within(root, path string) bool {
	rel, e := filepath.Rel(root, path)
	return e == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func loadConfig(path string) (config, error) {
	var c config
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 65537))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("expected one configuration object")
	}
	for _, p := range []string{c.Repository, c.KeyFile, c.StateDir} {
		if !filepath.IsAbs(p) {
			return c, errors.New("repository, key_file and state_dir must be absolute paths")
		}
	}
	privatePaths := []string{c.Repository, c.KeyFile, c.StateDir}
	for i, p := range privatePaths {
		a, e := resolve(p)
		if e != nil {
			return c, e
		}
		for _, q := range privatePaths[i+1:] {
			b, e := resolve(q)
			if e != nil {
				return c, e
			}
			if within(a, b) || within(b, a) {
				return c, errors.New("repository, key and agent state paths must not overlap")
			}
		}
	}
	// Restoring the repository remains possible even if the original volume is absent.
	if c.Volume.MountPoint != "" {
		root, e := resolve(c.Volume.MountPoint)
		if e != nil {
			return c, e
		}
		for _, p := range []string{c.Repository, c.KeyFile, c.StateDir} {
			private, e := resolve(p)
			if e != nil {
				return c, e
			}
			if within(root, private) || within(private, root) {
				return c, errors.New("repository, key and state must be separate from the protected volume mount")
			}
		}
	}
	return c, nil
}
func openStore(c config) (*blockstore.Store, error) {
	key, e := blockstore.LoadKeyFile(c.KeyFile)
	if e != nil {
		return nil, e
	}
	return blockstore.Open(c.Repository, key)
}
func takeLock(dir string) (func(), error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	lock := filepath.Join(dir, "operation.lock")
	if e := os.Mkdir(lock, 0700); e != nil {
		return nil, errors.New("agent is locked; if a previous process crashed, inspect driver and repository before manually removing operation.lock")
	}
	return func() { _ = os.Remove(lock) }, nil
}
func copyOracle(ctx context.Context, src *os.File, size int64, target string) error {
	if !filepath.IsAbs(target) {
		return errors.New("oracle target must be absolute")
	}
	f, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for offset := int64(0); offset < size; {
		if e = ctx.Err(); e != nil {
			return e
		}
		n := int64(len(buf))
		if n > size-offset {
			n = size - offset
		}
		data := buf[:int(n)]
		if _, e = src.ReadAt(data, offset); e != nil {
			return e
		}
		if _, e = f.Write(data); e != nil {
			return e
		}
		offset += n
	}
	return f.Sync()
}
func backup(ctx context.Context, c config, s *blockstore.Store, t tracker, oracle string) (backupResult, error) {
	var result backupResult
	if oracle != "" && os.Getenv("OPEN_VAULT_CBT_LAB") != "1" {
		return result, errors.New("oracle copying is laboratory-only; requires OPEN_VAULT_CBT_LAB=1")
	}
	parent, e := s.Head()
	if e != nil {
		return result, e
	}
	var previous blockstore.Generation
	if parent != "" {
		previous, e = s.Info(parent)
		if e != nil {
			return result, e
		}
	}
	tr, e := t.Current(ctx, c.Volume.ID)
	if e != nil {
		// Only a genuinely unconfigured driver journal can start a new baseline.
		if !errors.Is(e, cbt.ErrNoGeneration) {
			return result, e
		}
		if parent != "" {
			return result, errors.New("tracking state missing for existing backup chain; create a new repository and baseline after operator review")
		}
		size, e := t.SourceSize(ctx, c.Volume.ID)
		if e != nil {
			return result, e
		}
		if e = s.CheckAppend("", size); e != nil {
			return result, e
		}
		g, e := t.Setup(ctx, c.Volume.ID, randomName())
		if e != nil {
			return result, e
		}
		tr = cbt.Transition{Current: g}
	} else if tr.Current.Mode == "incremental" {
		g := tr.Current
		if parent == "" || previous.SourceID != c.Volume.ID || previous.TrackerUUID != g.UUID || previous.Size != g.Size || previous.Sequence != g.Sequence {
			return result, errors.New("committed parent does not match active tracking; refusing to rotate snapshot")
		}
		if e = s.CheckAppend(parent, g.Size); e != nil {
			return result, e
		}
		tr, e = t.ToSnapshot(ctx, c.Volume.ID, randomName())
		if e != nil {
			return result, e
		}
	}
	g := tr.Current
	if g.Mode != "snapshot" || g.Size <= 0 {
		return result, errors.New("driver did not provide a valid snapshot")
	}
	if e = t.Verify(ctx, c.Volume.ID, g); e != nil {
		return result, e
	}
	if parent != "" {
		if previous.SourceID != c.Volume.ID || previous.TrackerUUID != g.UUID || previous.Size != g.Size {
			return result, errors.New("source identity, tracking series or volume size changed; new baseline required")
		}
		if previous.Sequence == g.Sequence {
			// The image commit survived but tracker finalization did not. Do not create
			// another generation or advance the tracker twice.
			if e = s.ConfirmHead(ctx, parent); e != nil {
				return result, e
			}
			if e = t.ToIncremental(ctx, c.Volume.ID, g); e != nil {
				return result, &publishedError{previous, "confirmed", e}
			}
			result.Generation = previous
			result.FinalizedExisting = true
			return result, nil
		}
		if previous.Sequence+1 != g.Sequence || tr.PreviousSequence != previous.Sequence || tr.PreviousCOW == "" {
			return result, errors.New("tracking epoch does not match committed parent; refusing incomplete incremental")
		}
	}
	if e = s.CheckAppend(parent, g.Size); e != nil {
		return result, e
	}
	req := blockstore.Request{Parent: parent, Size: g.Size, SourceID: c.Volume.ID, TrackerUUID: g.UUID, Sequence: g.Sequence}
	if parent != "" {
		index, e := os.Open(tr.PreviousCOW)
		if e != nil {
			return result, e
		}
		ranges, e := cbt.ReadChanges(index, g.Size, g.UUID, tr.PreviousSequence)
		_ = index.Close()
		if e != nil {
			return result, e
		}
		for _, r := range ranges {
			req.Changed = append(req.Changed, blockstore.Extent{Offset: r.Offset, Length: r.Length})
		}
		result.TrackingIndexReadBytes = 4096 + ((g.Size+4095)/4096)*8
	}
	if e = blockstore.ValidateCapacity(req); e != nil {
		return result, e
	}
	source, e := os.Open(g.SnapshotDevice)
	if e != nil {
		return result, e
	}
	defer source.Close()
	if oracle != "" {
		if e = copyOracle(ctx, source, g.Size, oracle); e != nil {
			return result, e
		}
		result.OracleReadBytes = g.Size
	}
	if e = t.Verify(ctx, c.Volume.ID, g); e != nil {
		return result, e
	}
	result.Generation, e = s.Commit(ctx, source, req)
	if e != nil {
		if errors.Is(e, blockstore.ErrCommitUncertain) {
			return result, &publishedError{result.Generation, "uncertain", e}
		}
		return result, e
	}
	result.ThisRunSourceReadBytes = result.Generation.SourceReadBytes
	// The kernel refuses to retire a snapshot device while it has open readers.
	if e = source.Close(); e != nil {
		return result, &publishedError{result.Generation, "confirmed", e}
	}
	// A failed driver after the snapshot was read must not be silently declared
	// healthy. Keep its state and image so the operator can inspect the failure.
	if e = t.Verify(ctx, c.Volume.ID, g); e != nil {
		return result, &publishedError{result.Generation, "confirmed", fmt.Errorf("driver verification failed: %w", e)}
	}
	if e = t.ToIncremental(ctx, c.Volume.ID, g); e != nil {
		return result, &publishedError{result.Generation, "confirmed", fmt.Errorf("tracking finalization failed; inspect pending journal before retrying: %w", e)}
	}
	return result, nil
}
func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("open-vault-cbt", flag.ContinueOnError)
	configPath := flags.String("config", "config.cbt.json", "configuration file")
	generation := flags.String("generation", "", "generation ID; defaults to HEAD")
	target := flags.String("target", "", "new regular image file for restore")
	oracle := flags.String("oracle-target", "", "laboratory-only full snapshot copy")
	if e := flags.Parse(args); e != nil {
		return e
	}
	if flags.NArg() != 1 {
		return errors.New("usage: open-vault-cbt [flags] diagnose|backup|status|generations|restore|verify")
	}
	c, e := loadConfig(*configPath)
	if e != nil {
		return e
	}
	op := flags.Arg(0)
	if op == "backup" {
		if e = validateDiskSeparation(c, *oracle); e != nil {
			return e
		}
	}
	if op == "diagnose" {
		d, e := cbt.NewDriver(cbt.DriverConfig{DBDCtl: c.DBDCtl, StateFile: filepath.Join(c.StateDir, "driver.json"), Volumes: []cbt.Volume{c.Volume}})
		if e != nil {
			return e
		}
		report, e := d.Diagnose(ctx)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	s, e := openStore(c)
	if e != nil {
		return e
	}
	switch op {
	case "generations":
		items, e := s.Generations(ctx)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"generations": items})
	case "backup":
		unlock, e := takeLock(c.StateDir)
		if e != nil {
			return e
		}
		defer unlock()
		releaseRepo, e := takeLock(c.Repository)
		if e != nil {
			return e
		}
		defer releaseRepo()
		d, e := cbt.NewDriver(cbt.DriverConfig{DBDCtl: c.DBDCtl, StateFile: filepath.Join(c.StateDir, "driver.json"), Volumes: []cbt.Volume{c.Volume}})
		if e != nil {
			return e
		}
		result, e := backup(ctx, c, s, d, *oracle)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "status", "restore", "verify":
		id := *generation
		if id == "" {
			id, e = s.Head()
			if e != nil {
				return e
			}
		}
		if id == "" {
			return errors.New("repository has no generations")
		}
		switch op {
		case "restore":
			if *target == "" {
				return errors.New("restore requires -target for a new regular image file")
			}
			if e = s.Restore(ctx, id, *target); e != nil {
				return e
			}
		case "verify":
			if e = s.Verify(ctx, id); e != nil {
				return e
			}
		}
		info, e := s.Info(id)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"generation": info, "operation": op})
	default:
		return errors.New("unknown operation")
	}
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e := run(ctx, os.Args[1:]); e != nil {
		out := map[string]any{"error": e.Error()}
		var published *publishedError
		if errors.As(e, &published) {
			out["generation"] = published.Generation
			out["committed"] = true
			out["durability"] = published.Durability
		}
		_ = json.NewEncoder(os.Stderr).Encode(out)
		os.Exit(1)
	}
}
