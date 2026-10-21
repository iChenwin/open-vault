// Package blockstore stores authenticated block generations from stable snapshots.
// Changed extents come from the caller's change tracker; this package never scans
// a source to discover changes. It does not create snapshots or access devices.
package blockstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const chunkSize = 1 << 20
const manifestLimit = 64 << 20
const maxChain = 128
const manifestMagic = "OVBS1\n"

// ErrCommitUncertain means HEAD was atomically published but its directory sync
// failed. The returned Generation identifies the committed data; a coordinator
// must successfully ConfirmHead before acknowledging the tracker sequence. All other
// Commit errors occur before HEAD publication and leave HEAD unchanged.
var ErrCommitUncertain = errors.New("generation published but HEAD durability is uncertain")

// Extent identifies bytes within a snapshot. Length must be positive.
type Extent struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// Request creates a full baseline when Parent is empty. Otherwise Changed must
// contain exactly the ranges supplied by a trusted change tracker. An empty
// incremental Changed list is valid and reads zero source bytes.
type Request struct {
	Parent      string
	Size        int64
	Changed     []Extent
	SourceID    string
	TrackerUUID string
	Sequence    uint64
}

type Generation struct {
	ID              string    `json:"id"`
	Parent          string    `json:"parent,omitempty"`
	Size            int64     `json:"size"`
	ImageSHA256     string    `json:"image_sha256"`
	SourceReadBytes int64     `json:"source_read_bytes"`
	CreatedAt       time.Time `json:"created_at"`
	SourceID        string    `json:"source_id"`
	TrackerUUID     string    `json:"tracker_uuid"`
	Sequence        uint64    `json:"sequence"`
}

type record struct {
	Extent
	PayloadOffset int64  `json:"payload_offset"`
	CipherSize    int64  `json:"cipher_size"`
	SHA256        string `json:"sha256"`
}

type manifest struct {
	Generation
	Version int      `json:"version"`
	Records []record `json:"records"`
}

type Store struct {
	root string
	aead cipher.AEAD
	mu   sync.Mutex
}

// LoadKeyFile reads a hex-encoded 256-bit key, with optional surrounding space.
func LoadKeyFile(path string) ([]byte, error) {
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !linkInfo.Mode().IsRegular() {
		return nil, errors.New("blockstore key must be a regular file, not a symlink or device")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("blockstore key must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		return nil, errors.New("blockstore key file permissions must be 0600")
	}
	b, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(b))
	if len(s) != 64 {
		return nil, errors.New("blockstore key must contain exactly 64 hexadecimal characters")
	}
	key, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid blockstore key: %w", err)
	}
	return key, nil
}

func Open(root string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("blockstore requires a 32-byte AES-256 key")
	}
	if root == "" {
		return nil, errors.New("blockstore root is required")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &Store{root: root, aead: aead}, nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}

// Head returns an empty string for a repository without a published generation.
func (s *Store) Head() (string, error) {
	b, err := os.ReadFile(filepath.Join(s.root, "HEAD"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if !validID(id) {
		return "", errors.New("invalid blockstore HEAD")
	}
	return id, nil
}

// Info reads authenticated generation metadata. Empty id selects HEAD.
func (s *Store) Info(id string) (Generation, error) {
	if id == "" {
		var err error
		id, err = s.Head()
		if err != nil {
			return Generation{}, err
		}
		if id == "" {
			return Generation{}, errors.New("blockstore has no published generation")
		}
	}
	l, err := s.readLayer(id)
	if err != nil {
		return Generation{}, err
	}
	defer l.payload.Close()
	return l.m.Generation, nil
}

// ConfirmHead verifies the complete published image and repeats the HEAD file
// and repository directory syncs before a coordinator releases its snapshot.
// It resolves a prior ErrCommitUncertain without reading the source device or
// creating another generation. The caller must still hold its process-wide
// repository lock; mu serializes against commits through this Store instance.
func (s *Store) ConfirmHead(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil {
		return errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(id) {
		return errors.New("confirmation requires an explicit generation ID")
	}
	head, err := s.Head()
	if err != nil {
		return err
	}
	if head != id {
		return errors.New("generation is not current HEAD; refusing confirmation")
	}
	if err = s.Verify(ctx, id); err != nil {
		return fmt.Errorf("cannot confirm HEAD image: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// O_RDWR permits FlushFileBuffers in Windows development tests. No writes
	// occur here; HEAD must already exist and be a regular file.
	f, err := os.OpenFile(filepath.Join(s.root, "HEAD"), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("HEAD must be a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 1024))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != id {
		return errors.New("HEAD changed during confirmation")
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("cannot sync HEAD confirmation: %w", err)
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = syncDir(s.root); err != nil {
		return fmt.Errorf("cannot sync HEAD directory confirmation: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	head, err = s.Head()
	if err != nil {
		return err
	}
	if head != id {
		return errors.New("HEAD changed during confirmation")
	}
	return nil
}

func hashHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (s *Store) seal(plain, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, aad), nil
}
func (s *Store) unseal(b, aad []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(b) < n+s.aead.Overhead() {
		return nil, errors.New("truncated encrypted blockstore data")
	}
	plain, err := s.aead.Open(nil, b[:n], b[n:], aad)
	if err != nil {
		return nil, errors.New("blockstore authentication failed (wrong key or corrupted data)")
	}
	return plain, nil
}
func manifestAAD(id string) []byte { return []byte("open-vault/blockstore/v1/manifest/" + id) }
func recordAAD(id string, r record) []byte {
	return []byte(fmt.Sprintf("open-vault/blockstore/v1/payload/%s/%d/%d/%d", id, r.Offset, r.Length, r.PayloadOffset))
}

func extents(req Request) ([]Extent, error) {
	if req.Size <= 0 {
		return nil, errors.New("snapshot size must be positive")
	}
	if req.SourceID == "" || req.TrackerUUID == "" || req.Sequence == 0 {
		return nil, errors.New("source ID, tracker UUID and a positive sequence are required")
	}
	if req.Parent == "" {
		if len(req.Changed) != 0 {
			return nil, errors.New("baseline must not supply incremental extents")
		}
		return []Extent{{Offset: 0, Length: req.Size}}, nil
	}
	if !validID(req.Parent) {
		return nil, errors.New("invalid parent generation ID")
	}
	out := append([]Extent(nil), req.Changed...)
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	var end int64
	for i, e := range out {
		if e.Offset < 0 || e.Length <= 0 || e.Offset >= req.Size || e.Length > req.Size-e.Offset {
			return nil, errors.New("changed extent is outside the snapshot")
		}
		if i > 0 && e.Offset < end {
			return nil, errors.New("changed extents overlap")
		}
		end = e.Offset + e.Length
	}
	return out, nil
}

// Commit reads only the specified source ranges, authenticates all stored data,
// computes a complete reconstructed image hash, and publishes HEAD last. The
// caller must keep src stable for the whole call. Commits on one Store serialize;
// the repository must have only one writer process.
func (s *Store) Commit(ctx context.Context, src io.ReaderAt, req Request) (Generation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero Generation
	if ctx == nil {
		return zero, errors.New("context is required")
	}
	if err := ValidateCapacity(req); err != nil {
		return zero, err
	}
	ranges, err := extents(req)
	if err != nil {
		return zero, err
	}
	if src == nil && len(ranges) > 0 {
		return zero, errors.New("source reader is required")
	}
	var parents []*layer
	if req.Parent != "" {
		head, headErr := s.Head()
		if headErr != nil {
			return zero, headErr
		}
		if req.Parent != head {
			return zero, errors.New("incremental parent must equal current HEAD")
		}
		parents, err = s.chain(req.Parent)
		if err != nil {
			return zero, err
		}
		defer closeLayers(parents)
		if len(parents) >= maxChain {
			return zero, errors.New("generation chain limit reached; create a new baseline")
		}
		if parents[len(parents)-1].m.Size != req.Size {
			return zero, errors.New("snapshot size differs from parent; create a new baseline")
		}
		parent := parents[len(parents)-1].m
		if parent.SourceID != req.SourceID || parent.TrackerUUID != req.TrackerUUID {
			return zero, errors.New("incremental source ID or tracker UUID differs from parent")
		}
		if req.Sequence <= parent.Sequence {
			return zero, errors.New("incremental tracker sequence must strictly increase")
		}
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return zero, err
	}
	id := hex.EncodeToString(random)
	stage, err := os.MkdirTemp(s.root, ".pending-")
	if err != nil {
		return zero, err
	}
	defer os.RemoveAll(stage)
	payload, err := os.OpenFile(filepath.Join(stage, "payload"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return zero, err
	}
	defer payload.Close()
	m := manifest{Generation: Generation{ID: id, Parent: req.Parent, Size: req.Size, CreatedAt: time.Now().UTC(), SourceID: req.SourceID, TrackerUUID: req.TrackerUUID, Sequence: req.Sequence}, Version: 1, Records: []record{}}
	var payloadOffset int64
	buf := make([]byte, chunkSize)
	for _, extent := range ranges {
		for off, left := extent.Offset, extent.Length; left > 0; {
			if err = ctx.Err(); err != nil {
				return zero, err
			}
			length := min(int64(chunkSize), left)
			plain := buf[:int(length)]
			n, readErr := src.ReadAt(plain, off)
			m.SourceReadBytes += int64(n)
			if readErr != nil || n != len(plain) {
				if readErr == nil {
					readErr = io.ErrUnexpectedEOF
				}
				return zero, fmt.Errorf("read snapshot at %d: %w", off, readErr)
			}
			r := record{Extent: Extent{Offset: off, Length: length}, PayloadOffset: payloadOffset, SHA256: hashHex(plain)}
			sealed, sealErr := s.seal(plain, recordAAD(id, r))
			if sealErr != nil {
				return zero, sealErr
			}
			r.CipherSize = int64(len(sealed))
			if _, err = payload.Write(sealed); err != nil {
				return zero, err
			}
			m.Records = append(m.Records, r)
			payloadOffset += r.CipherSize
			off += length
			left -= length
		}
	}
	if err = payload.Sync(); err != nil {
		return zero, err
	}
	current := &layer{m: m, payload: payload}
	layers := append(parents, current)
	imageHash, err := s.stream(ctx, layers, io.Discard)
	if err != nil {
		return zero, err
	}
	m.ImageSHA256 = imageHash
	plainManifest, err := json.Marshal(m)
	if err != nil {
		return zero, err
	}
	if len(plainManifest) > manifestLimit {
		return zero, errors.New("manifest exceeds supported size")
	}
	sealed, err := s.seal(plainManifest, manifestAAD(id))
	if err != nil {
		return zero, err
	}
	if err = writeSynced(filepath.Join(stage, "manifest.enc"), append([]byte(manifestMagic), sealed...)); err != nil {
		return zero, err
	}
	if err = payload.Close(); err != nil {
		return zero, err
	}
	if err = syncDir(stage); err != nil {
		return zero, err
	}
	if err = ctx.Err(); err != nil {
		return zero, err
	}
	if err = os.Rename(stage, filepath.Join(s.root, id)); err != nil {
		return zero, err
	}
	if err = syncDir(s.root); err != nil {
		return zero, err
	}
	// A failed HEAD replacement can leave an unreferenced immutable generation;
	// it cannot advance HEAD or make a partially written generation visible.
	if err = s.publishHead(id); err != nil {
		if errors.Is(err, ErrCommitUncertain) {
			return m.Generation, err
		}
		return zero, err
	}
	return m.Generation, nil
}

func writeSynced(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
func (s *Store) publishHead(id string) error {
	f, err := os.CreateTemp(s.root, ".head-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.WriteString(id + "\n"); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(s.root, "HEAD")); err != nil {
		return err
	}
	if err = syncDir(s.root); err != nil {
		return fmt.Errorf("%w: %v", ErrCommitUncertain, err)
	}
	return nil
}

// Windows is supported for development tests; Linux ext4/XFS is the durability
// target. Go does not expose directory fsync on Windows through os.File.Sync.
func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type layer struct {
	m       manifest
	payload *os.File
}

func closeLayers(layers []*layer) {
	for _, l := range layers {
		l.payload.Close()
	}
}

func (s *Store) readLayer(id string) (*layer, error) {
	if !validID(id) {
		return nil, errors.New("invalid generation ID")
	}
	f, err := os.Open(filepath.Join(s.root, id, "manifest.enc"))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, manifestLimit+1024))
	f.Close()
	if err != nil {
		return nil, err
	}
	if len(b) > manifestLimit+128 || !strings.HasPrefix(string(b), manifestMagic) {
		return nil, errors.New("invalid encrypted manifest")
	}
	plain, err := s.unseal(b[len(manifestMagic):], manifestAAD(id))
	if err != nil {
		return nil, err
	}
	var m manifest
	if err = json.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Version != 1 || m.ID != id || m.Size <= 0 || !validHash(m.ImageSHA256) || m.SourceReadBytes < 0 || m.SourceID == "" || m.TrackerUUID == "" || m.Sequence == 0 {
		return nil, errors.New("invalid generation manifest")
	}
	if m.Parent != "" && !validID(m.Parent) {
		return nil, errors.New("invalid manifest parent")
	}
	var end, stored, readBytes int64
	for i, r := range m.Records {
		if r.Offset < 0 || r.Length <= 0 || r.Length > chunkSize || r.Offset >= m.Size || r.Length > m.Size-r.Offset || (i > 0 && r.Offset < end) || r.PayloadOffset != stored || r.CipherSize != r.Length+int64(s.aead.NonceSize()+s.aead.Overhead()) || !validHash(r.SHA256) {
			return nil, errors.New("invalid manifest payload record")
		}
		if m.Parent == "" && r.Offset != end {
			return nil, errors.New("baseline has a missing range")
		}
		end = r.Offset + r.Length
		stored += r.CipherSize
		readBytes += r.Length
	}
	if (m.Parent == "" && end != m.Size) || readBytes != m.SourceReadBytes {
		return nil, errors.New("incomplete generation manifest")
	}
	p, err := os.Open(filepath.Join(s.root, id, "payload"))
	if err != nil {
		return nil, err
	}
	info, err := p.Stat()
	if err != nil {
		p.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != stored {
		p.Close()
		return nil, errors.New("invalid payload file size or type")
	}
	return &layer{m: m, payload: p}, nil
}
func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil && h == strings.ToLower(h)
}

func (s *Store) chain(id string) ([]*layer, error) {
	if id == "" {
		var err error
		id, err = s.Head()
		if err != nil {
			return nil, err
		}
		if id == "" {
			return nil, errors.New("blockstore has no published generation")
		}
	}
	layers := []*layer{}
	seen := map[string]bool{}
	var size int64
	for id != "" {
		if seen[id] || len(layers) >= maxChain {
			closeLayers(layers)
			return nil, errors.New("generation chain cycle or limit exceeded")
		}
		seen[id] = true
		l, err := s.readLayer(id)
		if err != nil {
			closeLayers(layers)
			return nil, err
		}
		layers = append(layers, l)
		if size != 0 && size != l.m.Size {
			closeLayers(layers)
			return nil, errors.New("generation chain size mismatch")
		}
		size = l.m.Size
		id = l.m.Parent
	}
	for i, j := 0, len(layers)-1; i < j; i, j = i+1, j-1 {
		layers[i], layers[j] = layers[j], layers[i]
	}
	for i := 1; i < len(layers); i++ {
		p, c := layers[i-1].m, layers[i].m
		if p.SourceID != c.SourceID || p.TrackerUUID != c.TrackerUUID || p.Sequence >= c.Sequence {
			closeLayers(layers)
			return nil, errors.New("generation chain source or sequence mismatch")
		}
	}
	return layers, nil
}

func (s *Store) readRecord(l *layer, r record) ([]byte, error) {
	b := make([]byte, int(r.CipherSize))
	if _, err := l.payload.ReadAt(b, r.PayloadOffset); err != nil {
		return nil, err
	}
	plain, err := s.unseal(b, recordAAD(l.m.ID, r))
	if err != nil {
		return nil, err
	}
	if int64(len(plain)) != r.Length || hashHex(plain) != r.SHA256 {
		return nil, errors.New("payload SHA-256 mismatch")
	}
	return plain, nil
}

// stream overlays layers oldest-first in bounded memory. All contributing
// records, even those later overwritten, are authenticated and hash checked.
func (s *Store) stream(ctx context.Context, layers []*layer, dst io.Writer) (string, error) {
	if len(layers) == 0 {
		return "", errors.New("empty generation chain")
	}
	size := layers[len(layers)-1].m.Size
	h := sha256.New()
	out := io.MultiWriter(dst, h)
	buf := make([]byte, chunkSize)
	for off := int64(0); off < size; {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n := min(int64(chunkSize), size-off)
		block := buf[:int(n)]
		clear(block)
		for _, l := range layers {
			records := l.m.Records
			start := sort.Search(len(records), func(i int) bool { return records[i].Offset+records[i].Length > off })
			for i := start; i < len(records) && records[i].Offset < off+n; i++ {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				r := records[i]
				plain, err := s.readRecord(l, r)
				if err != nil {
					return "", fmt.Errorf("generation %s offset %d: %w", l.m.ID, r.Offset, err)
				}
				begin, end := max(off, r.Offset), min(off+n, r.Offset+r.Length)
				copy(block[begin-off:end-off], plain[begin-r.Offset:end-r.Offset])
			}
		}
		if _, err := out.Write(block); err != nil {
			return "", err
		}
		off += n
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify authenticates the complete chain and checks the complete image hash.
// Empty id selects HEAD. It never reads the original snapshot source.
func (s *Store) Verify(ctx context.Context, id string) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	layers, err := s.chain(id)
	if err != nil {
		return err
	}
	defer closeLayers(layers)
	actual, err := s.stream(ctx, layers, io.Discard)
	if err != nil {
		return err
	}
	if actual != layers[len(layers)-1].m.ImageSHA256 {
		return errors.New("restored image SHA-256 mismatch")
	}
	return nil
}

// Restore creates a new regular image file, validates the complete image, and
// removes a partial output on failure. Existing files, symlinks and devices are
// refused through O_EXCL; this method never opens an existing target for writing.
func (s *Store) Restore(ctx context.Context, id, target string) (err error) {
	if ctx == nil {
		return errors.New("context is required")
	}
	layers, err := s.chain(id)
	if err != nil {
		return err
	}
	defer closeLayers(layers)
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("restore requires a new regular file: %w", err)
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			os.Remove(target)
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("restore target must be a regular file; device writes are forbidden")
	}
	actual, err := s.stream(ctx, layers, f)
	if err != nil {
		return err
	}
	if actual != layers[len(layers)-1].m.ImageSHA256 {
		return errors.New("restored image SHA-256 mismatch")
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	success = true
	return nil
}
