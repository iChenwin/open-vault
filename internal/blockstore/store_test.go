package blockstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var testKey = bytes.Repeat([]byte{0x37}, 32)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func request(size int, parent string, seq uint64, ext ...Extent) Request {
	return Request{Size: int64(size), Parent: parent, Changed: ext, SourceID: "disk-test", TrackerUUID: "tracker-test", Sequence: seq}
}
func commit(t *testing.T, s *Store, src io.ReaderAt, r Request) Generation {
	t.Helper()
	g, err := s.Commit(context.Background(), src, r)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

type countedReader struct {
	data    []byte
	allowed []Extent
	reads   []Extent
	count   int64
	err     error
}

func (c *countedReader) ReadAt(b []byte, off int64) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	allowed := false
	for _, r := range c.allowed {
		if off >= r.Offset && off+int64(len(b)) <= r.Offset+r.Length {
			allowed = true
			break
		}
	}
	if !allowed {
		return 0, errors.New("source read outside declared changed extents")
	}
	c.reads = append(c.reads, Extent{Offset: off, Length: int64(len(b))})
	n, err := bytes.NewReader(c.data).ReadAt(b, off)
	c.count += int64(n)
	return n, err
}

func TestBaselineIncrementalRestoreReadsOnlyDeclaredChanges(t *testing.T) {
	s := newTestStore(t)
	size := 3*chunkSize + 37
	original := make([]byte, size)
	for i := range original {
		original[i] = byte(i * 7 % 251)
	}
	baseReader := &countedReader{data: original, allowed: []Extent{{0, int64(size)}}}
	base := commit(t, s, baseReader, request(size, "", 1))
	if base.SourceReadBytes != int64(size) || baseReader.count != int64(size) {
		t.Fatal("baseline source read count mismatch")
	}
	changed := []Extent{{int64(chunkSize - 11), 35}, {int64(2*chunkSize + 7), int64(chunkSize + 19)}}
	current := append([]byte(nil), original...)
	expected := append([]byte(nil), original...)
	var changedBytes int64
	for _, e := range changed {
		for j := e.Offset; j < e.Offset+e.Length; j++ {
			current[j] ^= 0xa5
			expected[j] = current[j]
		}
		changedBytes += e.Length
	}
	// An undeclared source change must never be scanned or enter this generation.
	current[3] ^= 0xff
	reader := &countedReader{data: current, allowed: changed}
	inc := commit(t, s, reader, request(size, base.ID, 3, changed...))
	if reader.count != changedBytes || inc.SourceReadBytes != changedBytes {
		t.Fatalf("read %d/%d, want %d", reader.count, inc.SourceReadBytes, changedBytes)
	}
	if inc.ImageSHA256 != hashHex(expected) {
		t.Fatal("wrong complete image hash")
	}
	if reader.count >= int64(size) {
		t.Fatal("incremental scanned whole source")
	}
	// Empty change sets read no source bytes and still create an authenticated generation.
	zeroReader := &countedReader{data: current}
	last := commit(t, s, zeroReader, request(size, inc.ID, 4))
	if last.SourceReadBytes != 0 || zeroReader.count != 0 || last.ImageSHA256 != inc.ImageSHA256 {
		t.Fatal("empty incremental changed image or read source")
	}
	if err := s.Verify(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info("")
	if err != nil {
		t.Fatal(err)
	}
	if info != last {
		t.Fatalf("authenticated metadata mismatch: %+v", info)
	}
	if head, err := s.Head(); err != nil || head != last.ID {
		t.Fatalf("bad head %q: %v", head, err)
	}
	target := filepath.Join(t.TempDir(), "restored.img")
	if err := s.Restore(context.Background(), last.ID, target); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("restored content mismatch")
	}
	if reader.count != changedBytes {
		t.Fatal("verify/restore read snapshot source")
	}
	if err := s.Restore(context.Background(), last.ID, target); err == nil {
		t.Fatal("overwrote existing output")
	}
	after, _ := os.ReadFile(target)
	if !bytes.Equal(after, expected) {
		t.Fatal("existing output changed")
	}
	if err := s.Restore(context.Background(), last.ID, os.DevNull); err == nil {
		t.Fatal("accepted device target")
	}
	stored, _ := os.ReadFile(filepath.Join(s.root, base.ID, "payload"))
	if bytes.Contains(stored, original[:128]) {
		t.Fatal("plaintext found in encrypted payload")
	}
}

func TestInvalidRequestsDoNotReadOrAdvanceHead(t *testing.T) {
	s := newTestStore(t)
	base := commit(t, s, bytes.NewReader(make([]byte, 4096)), request(4096, "", 1))
	tests := []struct {
		name string
		edit func(*Request)
	}{
		{"negative offset", func(r *Request) { r.Changed = []Extent{{-1, 1}} }},
		{"zero length", func(r *Request) { r.Changed = []Extent{{0, 0}} }},
		{"negative length", func(r *Request) { r.Changed = []Extent{{0, -1}} }},
		{"past end", func(r *Request) { r.Changed = []Extent{{4095, 2}} }},
		{"overflow", func(r *Request) { r.Changed = []Extent{{1, 1<<63 - 1}} }},
		{"overlap", func(r *Request) { r.Changed = []Extent{{0, 10}, {9, 10}} }},
		{"same sequence", func(r *Request) { r.Sequence = 1 }},
		{"zero sequence", func(r *Request) { r.Sequence = 0 }},
		{"wrong source", func(r *Request) { r.SourceID = "another-disk" }},
		{"wrong tracker", func(r *Request) { r.TrackerUUID = "another-tracker" }},
		{"missing source", func(r *Request) { r.SourceID = "" }},
		{"size changed", func(r *Request) { r.Size++ }},
		{"non-head parent", func(r *Request) { r.Parent = strings.Repeat("a", 32) }},
		{"unsafe parent", func(r *Request) { r.Parent = "../escape" }},
		{"baseline with extents", func(r *Request) { r.Parent = "" }},
		{"zero size", func(r *Request) { r.Size = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := request(4096, base.ID, 2, Extent{0, 10})
			tt.edit(&r)
			source := &countedReader{data: make([]byte, 4096), allowed: []Extent{{0, 4096}}}
			if _, err := s.Commit(context.Background(), source, r); err == nil {
				t.Fatal("accepted invalid request")
			}
			if source.count != 0 {
				t.Fatal("read source before validating request")
			}
			if h, _ := s.Head(); h != base.ID {
				t.Fatal("failure advanced HEAD")
			}
		})
	}
	inc := commit(t, s, nil, request(4096, base.ID, 3))
	if _, err := s.Commit(context.Background(), nil, request(4096, base.ID, 4)); err == nil {
		t.Fatal("accepted old parent")
	}
	if h, _ := s.Head(); h != inc.ID {
		t.Fatal("old parent changed HEAD")
	}
}

func TestReadFailureAndCancellationDoNotPublish(t *testing.T) {
	s := newTestStore(t)
	base := commit(t, s, bytes.NewReader(make([]byte, 128)), request(128, "", 1))
	before, _ := os.ReadDir(s.root)
	failing := &countedReader{err: io.ErrUnexpectedEOF}
	if _, err := s.Commit(context.Background(), failing, request(128, base.ID, 2, Extent{0, 64})); err == nil {
		t.Fatal("ignored source failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Commit(ctx, bytes.NewReader(make([]byte, 128)), request(128, base.ID, 3, Extent{0, 64})); !errors.Is(err, context.Canceled) {
		t.Fatalf("wanted cancellation: %v", err)
	}
	if head, _ := s.Head(); head != base.ID {
		t.Fatal("failed commit advanced HEAD")
	}
	after, _ := os.ReadDir(s.root)
	if len(after) != len(before) {
		t.Fatal("failed commit left staging data")
	}
	output := filepath.Join(t.TempDir(), "canceled.img")
	if err := s.Restore(ctx, base.ID, output); !errors.Is(err, context.Canceled) {
		t.Fatalf("wanted canceled restore: %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("partial restore retained")
	}
	if err := s.Verify(ctx, base.ID); !errors.Is(err, context.Canceled) {
		t.Fatal("verify ignored cancellation")
	}
}

func flipByte(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestWrongKeyAndManifestTamper(t *testing.T) {
	s := newTestStore(t)
	g := commit(t, s, bytes.NewReader([]byte("private snapshot")), request(16, "", 1))
	wrong, err := Open(s.root, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wrong.Info(g.ID); err == nil {
		t.Fatal("wrong key authenticated manifest")
	}
	if err = wrong.Verify(context.Background(), g.ID); err == nil {
		t.Fatal("wrong key verified image")
	}
	flipByte(t, filepath.Join(s.root, g.ID, "manifest.enc"))
	if _, err = s.Info(g.ID); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	if err = s.Verify(context.Background(), g.ID); err == nil {
		t.Fatal("tampered manifest verified")
	}
}

func TestPayloadTamperAlsoChecksOverwrittenAncestors(t *testing.T) {
	s := newTestStore(t)
	g := commit(t, s, bytes.NewReader(bytes.Repeat([]byte{1}, 4096)), request(4096, "", 1))
	inc := commit(t, s, bytes.NewReader(bytes.Repeat([]byte{2}, 4096)), request(4096, g.ID, 2, Extent{0, 4096}))
	flipByte(t, filepath.Join(s.root, g.ID, "payload"))
	if err := s.Verify(context.Background(), inc.ID); err == nil {
		t.Fatal("overwritten corrupt ancestor accepted")
	}
	output := filepath.Join(t.TempDir(), "tampered.img")
	if err := s.Restore(context.Background(), inc.ID, output); err == nil {
		t.Fatal("corrupted chain restored")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed restore retained partial image")
	}
	if _, err := s.Commit(context.Background(), nil, request(4096, inc.ID, 3)); err == nil {
		t.Fatal("extended corrupted parent")
	}
	if head, _ := s.Head(); head != inc.ID {
		t.Fatal("failed verification advanced HEAD")
	}
}

func TestCompleteImageHashVerified(t *testing.T) {
	s := newTestStore(t)
	g := commit(t, s, bytes.NewReader([]byte("snapshot")), request(8, "", 1))
	l, err := s.readLayer(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	l.payload.Close()
	l.m.ImageSHA256 = strings.Repeat("0", 64)
	plain, err := json.Marshal(l.m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.seal(plain, manifestAAD(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.root, g.ID, "manifest.enc"), append([]byte(manifestMagic), b...), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.Verify(context.Background(), g.ID); err == nil || !strings.Contains(err.Error(), "image SHA-256") {
		t.Fatalf("missing full-image checksum validation: %v", err)
	}
}

func TestMissingParentAndFailedHeadPublication(t *testing.T) {
	t.Run("missing parent", func(t *testing.T) {
		s := newTestStore(t)
		base := commit(t, s, bytes.NewReader(make([]byte, 512)), request(512, "", 1))
		inc := commit(t, s, nil, request(512, base.ID, 2))
		if err := os.RemoveAll(filepath.Join(s.root, base.ID)); err != nil {
			t.Fatal(err)
		}
		if err := s.Verify(context.Background(), inc.ID); err == nil {
			t.Fatal("missing ancestor accepted")
		}
	})
	t.Run("head publication failure", func(t *testing.T) {
		s := newTestStore(t)
		headPath := filepath.Join(s.root, "HEAD")
		if err := os.Mkdir(headPath, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(headPath, "keep"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Commit(context.Background(), bytes.NewReader(make([]byte, 512)), request(512, "", 1)); err == nil {
			t.Fatal("ignored HEAD publication error")
		}
		data, err := os.ReadFile(filepath.Join(headPath, "keep"))
		if err != nil || string(data) != "original" {
			t.Fatal("failed publication changed old HEAD")
		}
		entries, _ := os.ReadDir(s.root)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".pending-") || strings.HasPrefix(e.Name(), ".head-") {
				t.Fatal("left temporary publication files")
			}
		}
	})
}

func TestKeyFilesAndInvalidStoreInputs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(testKey)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := LoadKeyFile(path)
	if err != nil || !bytes.Equal(key, testKey) {
		t.Fatalf("valid key failed: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err = os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadKeyFile(path); err == nil {
			t.Fatal("accepted exposed key file")
		}
		os.Chmod(path, 0600)
	}
	for _, value := range []string{"", strings.Repeat("x", 64), strings.Repeat("0", 62), strings.Repeat("0", 66), strings.Repeat("0", 2048)} {
		if err = os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadKeyFile(path); err == nil {
			t.Fatal("accepted invalid key file")
		}
	}
	if _, err = Open(dir, testKey[:31]); err == nil {
		t.Fatal("accepted short key")
	}
	if _, err = Open("", testKey); err == nil {
		t.Fatal("accepted empty root")
	}
	s := newTestStore(t)
	if head, err := s.Head(); err != nil || head != "" {
		t.Fatal("empty repository has a HEAD")
	}
	if err = s.Verify(context.Background(), ""); err == nil {
		t.Fatal("verified empty repository")
	}
	if _, err = s.Info("../escape"); err == nil {
		t.Fatal("unsafe ID accepted")
	}
	if _, err = s.Commit(nil, nil, Request{}); err == nil {
		t.Fatal("nil context accepted")
	}
	if err = s.Verify(nil, ""); err == nil {
		t.Fatal("nil context accepted")
	}
	if err = s.Restore(nil, "", "output"); err == nil {
		t.Fatal("nil context accepted")
	}
}
