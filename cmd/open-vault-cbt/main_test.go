package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ichenwin/open-vault/internal/blockstore"
	"github.com/ichenwin/open-vault/internal/cbt"
)

type fakeTracker struct {
	current         cbt.Transition
	source, index   string
	size            int64
	finalizeFailure bool
	setups, rotates int
	currentError    error
}

const series = "00112233445566778899aabbccddeeff"

func (f *fakeTracker) Current(context.Context, string) (cbt.Transition, error) {
	if f.currentError != nil {
		return cbt.Transition{}, f.currentError
	}
	if f.current.Current.UUID == "" {
		return cbt.Transition{}, cbt.ErrNoGeneration
	}
	return f.current, nil
}
func (f *fakeTracker) Setup(context.Context, string, string) (cbt.Generation, error) {
	f.setups++
	f.current = cbt.Transition{Current: cbt.Generation{VolumeID: "data", UUID: series, Sequence: 1, Mode: "snapshot", SnapshotDevice: f.source, Size: f.size}}
	return f.current.Current, nil
}
func (f *fakeTracker) ToSnapshot(context.Context, string, string) (cbt.Transition, error) {
	f.rotates++
	f.current.PreviousSequence = f.current.Current.Sequence
	f.current.PreviousCOW = f.index
	f.current.Current.Sequence++
	f.current.Current.Mode = "snapshot"
	return f.current, nil
}
func (f *fakeTracker) ToIncremental(_ context.Context, _ string, g cbt.Generation) error {
	if f.finalizeFailure {
		f.finalizeFailure = false
		return errors.New("simulated driver command failure")
	}
	if g.Sequence != f.current.Current.Sequence {
		return errors.New("stale generation")
	}
	f.current.Current.Mode = "incremental"
	return nil
}
func (f *fakeTracker) Verify(_ context.Context, _ string, g cbt.Generation) error {
	if g.UUID != f.current.Current.UUID || g.Sequence != f.current.Current.Sequence || g.Mode != f.current.Current.Mode {
		return errors.New("driver changed")
	}
	return nil
}
func coordinatorFixture(t *testing.T) (config, *blockstore.Store, *fakeTracker, []byte) {
	t.Helper()
	root := t.TempDir()
	data := bytes.Repeat([]byte{42}, 4*4096)
	source := filepath.Join(root, "source-image")
	if e := os.WriteFile(source, data, 0600); e != nil {
		t.Fatal(e)
	}
	s, e := blockstore.Open(filepath.Join(root, "repo"), bytes.Repeat([]byte{5}, 32))
	if e != nil {
		t.Fatal(e)
	}
	return config{Volume: cbt.Volume{ID: "data"}}, s, &fakeTracker{source: source, size: int64(len(data)), index: filepath.Join(root, "changes")}, data
}
func writeIndex(t *testing.T, path string, seq uint64, blocks int, changed ...int) {
	t.Helper()
	data := make([]byte, 4096+blocks*8)
	binary.LittleEndian.PutUint32(data, 4776)
	binary.LittleEndian.PutUint32(data[4:], 3)
	binary.LittleEndian.PutUint64(data[24:], seq)
	u, _ := hex.DecodeString(series)
	copy(data[32:], u)
	for _, i := range changed {
		binary.LittleEndian.PutUint64(data[4096+i*8:], 1)
	}
	if e := os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestCoordinatorFullAndTrueIncrementalRanges(t *testing.T) {
	c, s, d, original := coordinatorFixture(t)
	ctx := context.Background()
	first, e := backup(ctx, c, s, d, "")
	if e != nil {
		t.Fatal(e)
	}
	if first.Generation.SourceReadBytes != int64(len(original)) {
		t.Fatal("baseline source counter")
	}
	updated := append([]byte(nil), original...)
	copy(updated[4096:8192], bytes.Repeat([]byte{99}, 4096))
	if e = os.WriteFile(d.source, updated, 0600); e != nil {
		t.Fatal(e)
	}
	writeIndex(t, d.index, 1, 4, 1)
	second, e := backup(ctx, c, s, d, "")
	if e != nil {
		t.Fatal(e)
	}
	if second.Generation.Parent != first.Generation.ID || second.Generation.SourceReadBytes != 4096 {
		t.Fatalf("delta did not use bitmap: %+v", second)
	}
	out := filepath.Join(t.TempDir(), "restore.img")
	if e = s.Restore(ctx, second.Generation.ID, out); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(out)
	if e != nil || !bytes.Equal(got, updated) {
		t.Fatal("delta reconstruction mismatch", e)
	}
	old := filepath.Join(t.TempDir(), "old.img")
	if e = s.Restore(ctx, first.Generation.ID, old); e != nil {
		t.Fatal(e)
	}
	got, _ = os.ReadFile(old)
	if !bytes.Equal(got, original) {
		t.Fatal("baseline mutated")
	}
}
func TestCommitBeforeTrackerFinalizationIsRecoverable(t *testing.T) {
	c, s, d, _ := coordinatorFixture(t)
	d.finalizeFailure = true
	if _, e := backup(context.Background(), c, s, d, ""); e == nil {
		t.Fatal("tracking failure hidden")
	}
	id, e := s.Head()
	if e != nil || id == "" {
		t.Fatal("image not committed")
	}
	result, e := backup(context.Background(), c, s, d, "")
	if e != nil {
		t.Fatal(e)
	}
	if !result.FinalizedExisting || result.Generation.ID != id || d.setups != 1 || d.rotates != 0 {
		t.Fatal("retry changed committed snapshot")
	}
}
func TestTrackerLossNeverResetsBaseline(t *testing.T) {
	c, s, d, _ := coordinatorFixture(t)
	if _, e := backup(context.Background(), c, s, d, ""); e != nil {
		t.Fatal(e)
	}
	d.current = cbt.Transition{}
	if _, e := backup(context.Background(), c, s, d, ""); e == nil {
		t.Fatal("accepted lost tracker")
	}
	if d.setups != 1 {
		t.Fatal("silently reset tracking")
	}
	d.currentError = errors.New("uncertain pending command")
	if _, e := backup(context.Background(), c, s, d, ""); e == nil {
		t.Fatal("accepted uncertain driver")
	}
	if d.setups != 1 {
		t.Fatal("setup after uncertain state")
	}
}
func TestInvalidBitmapLeavesCommittedParent(t *testing.T) {
	c, s, d, _ := coordinatorFixture(t)
	first, e := backup(context.Background(), c, s, d, "")
	if e != nil {
		t.Fatal(e)
	}
	writeIndex(t, d.index, 999, 4, 0)
	if _, e = backup(context.Background(), c, s, d, ""); e == nil {
		t.Fatal("accepted wrong bitmap epoch")
	}
	head, _ := s.Head()
	if head != first.Generation.ID || d.current.Current.Mode != "snapshot" {
		t.Fatal("failed bitmap advanced state")
	}
}
func TestProcessLockAndLabGate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	unlock, e := takeLock(dir)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = takeLock(dir); e == nil {
		t.Fatal("second writer allowed")
	}
	unlock()
	c, s, d, _ := coordinatorFixture(t)
	t.Setenv("OPEN_VAULT_CBT_LAB", "")
	if _, e = backup(context.Background(), c, s, d, filepath.Join(t.TempDir(), "oracle")); e == nil {
		t.Fatal("production oracle enabled")
	}
	head, _ := s.Head()
	if head != "" {
		t.Fatal("oracle gate advanced head")
	}
}
