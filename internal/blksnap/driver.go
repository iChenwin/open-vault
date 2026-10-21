// Package blksnap controls an independently installed, pinned blksnap CLI.
// It does not link or copy the upstream kernel, library or tool implementation.
package blksnap

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ichenwin/open-vault/internal/cbt"
)

const UpstreamCommit = "a162bc2064dbb1dbadf12bd1fc628bf82338d14f"
const ModuleVersion = "7.0.0.0"
const maxMapBytes = 64 << 20

type Config struct {
	Binary          string     `json:"binary"`
	StateDir        string     `json:"state_dir"`
	Volume          cbt.Volume `json:"volume"`
	DifferenceLimit string     `json:"difference_limit"`
}

type tracking struct {
	UUID      string `json:"uuid"`
	Sequence  uint64 `json:"sequence"`
	BlockSize int64  `json:"block_size"`
	Count     int64  `json:"count"`
	Size      int64  `json:"size"`
}
type entry struct {
	Current    cbt.Generation `json:"current"`
	Previous   uint64         `json:"previous_sequence"`
	Tracking   tracking       `json:"tracking"`
	SnapshotID string         `json:"snapshot_id"`
	BootID     string         `json:"boot_id"`
	Pending    string         `json:"pending,omitempty"`
}
type journal struct {
	Version int        `json:"version"`
	Volume  cbt.Volume `json:"volume"`
	Entry   *entry     `json:"entry"`
}

type Driver struct {
	mu       sync.Mutex
	cfg      Config
	command  func(context.Context, ...string) ([]byte, error)
	validate func(cbt.Volume) error
	size     func(string) (int64, error)
	platform func() (string, bool)
	boot     func() (string, error)
}

var uuidCLI = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var uuidRaw = regexp.MustCompile(`^[a-f0-9]{32}$`)
var safeName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)
var imagePath = regexp.MustCompile(`^/dev/vbsnap-[0-9]+-[0-9]+$`)
var limitPattern = regexp.MustCompile(`^[1-9][0-9]{0,10}[KMG]$`)
var decimal = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
var moduleVersionPattern = regexp.MustCompile(`^[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}$`)

// Diagnose never attaches a tracker, takes a snapshot or writes a journal.
// DeviceStatus describes dattobd's proc format, so blksnap does not fabricate
// entries in it; the active generation is exposed through Current instead.
func (d *Driver) Diagnose(ctx context.Context) (cbt.Report, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	kernel, supported := d.platform()
	r := cbt.Report{Provider: "blksnap", Supported: supported, Kernel: kernel, Architecture: runtime.GOARCH, Devices: []cbt.DeviceStatus{}}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if !supported {
		r.Problems = append(r.Problems, "tracking requires Ubuntu24.04 amd64 GA6.8")
	}
	if err := d.validate(d.cfg.Volume); err != nil {
		r.Problems = append(r.Problems, err.Error())
	}
	if _, err := trustedBinary(d.cfg.Binary); err != nil {
		r.Problems = append(r.Problems, err.Error())
		return r, nil
	}
	b, err := d.command(ctx, "version")
	if err != nil {
		r.Problems = append(r.Problems, "blksnap module query failed")
		return r, nil
	}
	r.DriverVersion = strings.TrimSpace(string(b))
	if !moduleVersionPattern.MatchString(r.DriverVersion) {
		r.DriverVersion = ""
		r.Problems = append(r.Problems, "invalid blksnap module query response")
		return r, nil
	}
	r.DriverLoaded = true
	if r.DriverVersion != ModuleVersion {
		r.Problems = append(r.Problems, "blksnap module version differs from the required build")
		return r, nil
	}
	if _, err := privateDir(d.cfg.StateDir); err != nil {
		r.Problems = append(r.Problems, err.Error())
		return r, nil
	}
	j, err := d.load()
	if err != nil {
		r.Problems = append(r.Problems, err.Error())
		return r, nil
	}
	if j.Entry != nil {
		if _, err = d.checked(ctx, j); err != nil {
			r.Problems = append(r.Problems, err.Error())
		}
	}
	return r, nil
}

func within(root, path string) bool {
	r, e := filepath.Rel(root, path)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}
func overlaps(a, b string) bool { return within(a, b) || within(b, a) }

func New(c Config) (*Driver, error) {
	if c.Binary == "" || !filepath.IsAbs(c.Binary) {
		return nil, errors.New("blksnap binary must be an absolute trusted path")
	}
	if c.DifferenceLimit == "" {
		c.DifferenceLimit = "1G"
	}
	if !limitPattern.MatchString(c.DifferenceLimit) {
		return nil, errors.New("invalid difference storage limit")
	}
	n, _ := strconv.ParseInt(c.DifferenceLimit[:len(c.DifferenceLimit)-1], 10, 64)
	shift := map[byte]uint{'K': 10, 'M': 20, 'G': 30}[c.DifferenceLimit[len(c.DifferenceLimit)-1]]
	if n > (64<<40)>>shift || n<<shift < 1<<20 {
		return nil, errors.New("difference storage limit outside supported bounds")
	}
	if !safeName.MatchString(c.Volume.ID) {
		return nil, errors.New("invalid approved volume identifier")
	}
	for _, p := range []string{c.StateDir, c.Volume.Device, c.Volume.MountPoint, c.Volume.COWDir} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, errors.New("blksnap paths must be clean absolute paths")
		}
	}
	var err error
	if c.Binary, err = trustedBinary(c.Binary); err != nil {
		return nil, err
	}
	if c.StateDir, err = privateDir(c.StateDir); err != nil {
		return nil, err
	}
	for _, p := range []*string{&c.Volume.Device, &c.Volume.MountPoint, &c.Volume.COWDir} {
		v, e := filepath.EvalSymlinks(*p)
		if e != nil {
			return nil, errors.New("approved volume paths must exist")
		}
		*p = v
	}
	if overlaps(c.StateDir, c.Volume.COWDir) || overlaps(c.StateDir, c.Volume.MountPoint) || within(c.Volume.MountPoint, c.Binary) || within(c.Volume.COWDir, c.Binary) || within(c.StateDir, c.Binary) {
		return nil, errors.New("state, executable, source and difference storage must not overlap")
	}
	d := &Driver{cfg: c, validate: cbt.ValidateExternalVolume, size: cbt.DataVolumeSize, platform: cbt.HostPlatform}
	d.boot = func() (string, error) {
		b, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if e != nil {
			return "", errors.New("cannot determine boot continuity")
		}
		v := strings.TrimSpace(string(b))
		if !uuidCLI.MatchString(v) {
			return "", errors.New("invalid boot identity")
		}
		return v, nil
	}
	d.command = d.run
	return d, nil
}

type capped struct {
	b        bytes.Buffer
	limit    int
	overflow bool
}

func (b *capped) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.b.Len()
	if len(p) > left {
		p = p[:left]
		b.overflow = true
	}
	_, _ = b.b.Write(p)
	return n, nil
}
func (d *Driver) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.Binary, args...)
	configureProcess(cmd)
	out, stderr := &capped{limit: 4 << 20}, &capped{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if e := cmd.Run(); e != nil {
		return nil, errors.New("blksnap command failed or timed out; inspect privileged driver diagnostics")
	}
	if out.overflow || stderr.overflow {
		return nil, errors.New("blksnap command output exceeded bounds")
	}
	return out.b.Bytes(), nil
}
func (d *Driver) begin(ctx context.Context, id string) (*journal, func(), error) {
	if e := ctx.Err(); e != nil {
		return nil, nil, e
	}
	if id != d.cfg.Volume.ID {
		return nil, nil, errors.New("volume is not approved")
	}
	if _, ok := d.platform(); !ok {
		return nil, nil, errors.New("blksnap requires Ubuntu24.04 amd64 GA6.8")
	}
	if _, e := trustedBinary(d.cfg.Binary); e != nil {
		return nil, nil, e
	}
	if _, e := privateDir(d.cfg.StateDir); e != nil {
		return nil, nil, e
	}
	if e := d.validate(d.cfg.Volume); e != nil {
		return nil, nil, e
	}
	lock := filepath.Join(d.cfg.StateDir, ".operation-lock")
	if e := os.Mkdir(lock, 0700); e != nil {
		return nil, nil, errors.New("driver state locked; inspect interrupted process before recovery")
	}
	unlock := func() { _ = os.Remove(lock) }
	j, e := d.load()
	if e != nil {
		unlock()
		return nil, nil, e
	}
	b, e := d.command(ctx, "version")
	if e != nil || strings.TrimSpace(string(b)) != ModuleVersion {
		unlock()
		return nil, nil, errors.New("required blksnap module version is unavailable")
	}
	return j, unlock, nil
}
func (d *Driver) statePath() string { return filepath.Join(d.cfg.StateDir, "tracker.json") }
func (d *Driver) load() (*journal, error) {
	j := &journal{Version: 1, Volume: d.cfg.Volume}
	st, e := os.Lstat(d.statePath())
	if os.IsNotExist(e) {
		return j, nil
	}
	if e != nil || !st.Mode().IsRegular() || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
		return nil, errors.New("driver journal is not a private regular file")
	}
	if e = secureOwner(st); e != nil {
		return nil, e
	}
	f, e := os.Open(d.statePath())
	if e != nil {
		return nil, errors.New("cannot open driver journal")
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if e != nil || len(b) > 1<<20 {
		return nil, errors.New("invalid driver journal size")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if e = dec.Decode(j); e != nil || dec.Decode(new(any)) != io.EOF || j.Version != 1 || j.Volume != d.cfg.Volume || j.Entry == nil {
		return nil, errors.New("invalid or mismatched driver journal; manual baseline required")
	}
	if j.Entry.Pending != "" {
		return nil, errors.New("driver operation is pending; manual inspection and baseline required")
	}
	return j, nil
}
func (d *Driver) save(j *journal) error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(d.cfg.StateDir, ".tracker-*")
	if e != nil {
		return errors.New("cannot prepare driver journal")
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return errors.New("cannot persist driver journal")
	}
	if e = os.Rename(name, d.statePath()); e != nil {
		return errors.New("cannot publish driver journal")
	}
	return syncDir(d.cfg.StateDir)
}
func (d *Driver) change(ctx context.Context, j *journal, phase string, args ...string) ([]byte, error) {
	j.Entry.Pending = phase
	if e := d.save(j); e != nil {
		return nil, e
	}
	return d.command(ctx, args...)
}
func parseFields(b []byte, expected ...string) (map[string]string, error) {
	if len(b) > 4096 {
		return nil, errors.New("driver metadata exceeds bounds")
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || v == "" || m[k] != "" {
			return nil, errors.New("invalid driver metadata")
		}
		m[k] = v
	}
	if len(m) != len(expected) {
		return nil, errors.New("unexpected driver metadata fields")
	}
	for _, key := range expected {
		if m[key] == "" {
			return nil, errors.New("missing driver metadata field")
		}
	}
	return m, nil
}
func (d *Driver) tracking(ctx context.Context) (tracking, error) {
	b, e := d.command(ctx, "cbtinfo", "--device", d.cfg.Volume.Device)
	if e != nil {
		return tracking{}, e
	}
	m, e := parseFields(b, "block_size", "device_capacity", "block_count", "generation_id", "changes_number")
	if e != nil {
		return tracking{}, e
	}
	if !uuidCLI.MatchString(m["generation_id"]) {
		return tracking{}, errors.New("invalid tracker UUID")
	}
	vals := map[string]int64{}
	for _, k := range []string{"block_size", "device_capacity", "block_count", "changes_number"} {
		if !decimal.MatchString(m[k]) {
			return tracking{}, errors.New("invalid numeric tracker metadata")
		}
		v, err := strconv.ParseInt(m[k], 10, 64)
		if err != nil {
			return tracking{}, errors.New("tracker number overflow")
		}
		vals[k] = v
	}
	t := tracking{UUID: strings.ReplaceAll(m["generation_id"], "-", ""), Sequence: uint64(vals["changes_number"]), BlockSize: vals["block_size"], Count: vals["block_count"], Size: vals["device_capacity"]}
	if t.Size <= 0 || t.Size > 64<<40 || t.Size%512 != 0 || t.BlockSize < 512 || t.BlockSize > 1<<30 || t.BlockSize&(t.BlockSize-1) != 0 || t.Count != (t.Size+t.BlockSize-1)/t.BlockSize || t.Count > maxMapBytes || t.Sequence > 255 {
		return tracking{}, errors.New("invalid tracker geometry or epoch")
	}
	size, e := d.size(d.cfg.Volume.Device)
	if e != nil || size != t.Size {
		return tracking{}, errors.New("source capacity changed or unavailable")
	}
	return t, nil
}
func (d *Driver) snapshot(ctx context.Context) (string, error) {
	b, e := d.command(ctx, "snapshot_info", "--device", d.cfg.Volume.Device)
	if e != nil {
		return "", e
	}
	m, e := parseFields(b, "error_code", "image")
	if e != nil {
		return "", e
	}
	if m["error_code"] != "0" {
		return "", errors.New("snapshot is corrupted; refusing image publication or finalization")
	}
	if m["image"] == "/dev/" {
		return "", nil
	}
	if !imagePath.MatchString(m["image"]) {
		return "", errors.New("unexpected snapshot device")
	}
	return m["image"], nil
}
func (d *Driver) collect(ctx context.Context, want string, exists bool) error {
	b, e := d.command(ctx, "snapshot_collect")
	if e != nil {
		return e
	}
	seen := map[string]bool{}
	for _, id := range strings.Fields(string(b)) {
		if !uuidCLI.MatchString(id) || seen[id] {
			return errors.New("invalid snapshot collection")
		}
		seen[id] = true
	}
	if seen[want] != exists {
		return errors.New("snapshot identity no longer matches journal")
	}
	return nil
}
func (d *Driver) checked(ctx context.Context, j *journal) (*entry, error) {
	e := j.Entry
	if e == nil {
		return nil, cbt.ErrNoGeneration
	}
	if e.Pending != "" || e.Current.VolumeID != d.cfg.Volume.ID || e.Current.Minor != d.cfg.Volume.Minor || e.Current.COWFile != "" || !uuidRaw.MatchString(e.Current.UUID) || e.Current.Sequence == 0 || e.Current.Sequence > 255 || e.Previous+1 != e.Current.Sequence {
		return nil, errors.New("invalid durable tracking generation")
	}
	boot, err := d.boot()
	if err != nil || boot != e.BootID {
		return nil, errors.New("boot continuity lost; new baseline required")
	}
	t, err := d.tracking(ctx)
	if err != nil {
		return nil, err
	}
	if t != e.Tracking || t.UUID != e.Current.UUID || t.Sequence != e.Current.Sequence || t.Size != e.Current.Size {
		return nil, errors.New("tracking generation changed; new baseline required")
	}
	image, err := d.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	switch e.Current.Mode {
	case "snapshot":
		if !uuidCLI.MatchString(e.SnapshotID) || image == "" || image != e.Current.SnapshotDevice {
			return nil, errors.New("snapshot no longer matches journal")
		}
		if err = d.collect(ctx, e.SnapshotID, true); err != nil {
			return nil, err
		}
		size, err := d.size(image)
		if err != nil || size != t.Size {
			return nil, errors.New("snapshot capacity does not match source")
		}
	case "incremental":
		if image != "" || e.SnapshotID != "" || e.Current.SnapshotDevice != "" {
			return nil, errors.New("unexpected snapshot while tracking incrementally")
		}
	default:
		return nil, errors.New("invalid tracking mode")
	}
	return e, nil
}
func transition(e *entry) cbt.Transition {
	return cbt.Transition{Current: e.Current, PreviousSequence: e.Previous}
}

func (d *Driver) SourceSize(ctx context.Context, id string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, unlock, e := d.begin(ctx, id)
	_ = j
	if e != nil {
		return 0, e
	}
	defer unlock()
	return d.size(d.cfg.Volume.Device)
}
func (d *Driver) Current(ctx context.Context, id string) (cbt.Transition, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, unlock, e := d.begin(ctx, id)
	if e != nil {
		return cbt.Transition{}, e
	}
	defer unlock()
	v, e := d.checked(ctx, j)
	if e != nil {
		return cbt.Transition{}, e
	}
	return transition(v), nil
}
func (d *Driver) Verify(ctx context.Context, id string, expected cbt.Generation) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, unlock, e := d.begin(ctx, id)
	if e != nil {
		return e
	}
	defer unlock()
	v, e := d.checked(ctx, j)
	if e != nil {
		return e
	}
	if v.Current != expected {
		return errors.New("generation differs from expected snapshot")
	}
	return nil
}
func (d *Driver) createSnapshot(ctx context.Context, j *journal, prior tracking) error {
	if prior.Sequence >= 255 {
		return errors.New("CBT epoch rollover requires a new baseline")
	}
	b, e := d.change(ctx, j, "create", "snapshot_create", "--device", d.cfg.Volume.Device, "--file", d.cfg.Volume.COWDir, "--limit", d.cfg.DifferenceLimit)
	if e != nil {
		return e
	}
	id := strings.TrimSpace(string(b))
	if !uuidCLI.MatchString(id) {
		return errors.New("invalid created snapshot identity; operation remains pending")
	}
	j.Entry.SnapshotID = id
	if _, e = d.change(ctx, j, "take", "snapshot_take", "--id", id); e != nil {
		return e
	}
	t, e := d.tracking(ctx)
	if e != nil {
		return e
	}
	if t.UUID != prior.UUID || t.Sequence != prior.Sequence+1 || t.Size != prior.Size || t.BlockSize != prior.BlockSize || t.Count != prior.Count {
		return errors.New("snapshot switched tracking generation unexpectedly")
	}
	image, e := d.snapshot(ctx)
	if e != nil {
		return e
	}
	if image == "" {
		return errors.New("snapshot image was not created")
	}
	if e = d.collect(ctx, id, true); e != nil {
		return e
	}
	size, e := d.size(image)
	if e != nil || size != t.Size {
		return errors.New("invalid snapshot capacity")
	}
	j.Entry.Current = cbt.Generation{VolumeID: d.cfg.Volume.ID, UUID: t.UUID, Sequence: t.Sequence, Mode: "snapshot", Size: t.Size, Minor: d.cfg.Volume.Minor, SnapshotDevice: image}
	j.Entry.Previous = prior.Sequence
	j.Entry.Tracking = t
	j.Entry.Pending = ""
	return d.save(j)
}
func (d *Driver) Setup(ctx context.Context, id, name string) (cbt.Generation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !safeName.MatchString(name) {
		return cbt.Generation{}, errors.New("invalid operation name")
	}
	j, unlock, e := d.begin(ctx, id)
	if e != nil {
		return cbt.Generation{}, e
	}
	defer unlock()
	if j.Entry != nil {
		return cbt.Generation{}, errors.New("tracking already initialized")
	}
	boot, e := d.boot()
	if e != nil {
		return cbt.Generation{}, e
	}
	j.Entry = &entry{BootID: boot}
	b, e := d.change(ctx, j, "attach", "attach", "--device", d.cfg.Volume.Device)
	if e != nil {
		return cbt.Generation{}, e
	}
	if strings.TrimSpace(string(b)) != "Attached successfully" {
		return cbt.Generation{}, errors.New("source tracker already existed or returned an unknown result; manual baseline required")
	}
	t, e := d.tracking(ctx)
	if e != nil {
		return cbt.Generation{}, e
	}
	if t.Sequence != 0 {
		return cbt.Generation{}, errors.New("new tracker has a nonzero epoch")
	}
	if e = d.createSnapshot(ctx, j, t); e != nil {
		return cbt.Generation{}, e
	}
	return j.Entry.Current, nil
}
func (d *Driver) ToSnapshot(ctx context.Context, id, name string) (cbt.Transition, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !safeName.MatchString(name) {
		return cbt.Transition{}, errors.New("invalid operation name")
	}
	j, unlock, e := d.begin(ctx, id)
	if e != nil {
		return cbt.Transition{}, e
	}
	defer unlock()
	v, e := d.checked(ctx, j)
	if e != nil {
		return cbt.Transition{}, e
	}
	if v.Current.Mode != "incremental" {
		return cbt.Transition{}, errors.New("snapshot transition requires incremental tracking mode")
	}
	if e = d.createSnapshot(ctx, j, v.Tracking); e != nil {
		return cbt.Transition{}, e
	}
	return transition(j.Entry), nil
}
func (d *Driver) ToIncremental(ctx context.Context, id string, expected cbt.Generation) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, unlock, e := d.begin(ctx, id)
	if e != nil {
		return e
	}
	defer unlock()
	v, e := d.checked(ctx, j)
	if e != nil {
		return e
	}
	if expected.Mode != "snapshot" || v.Current != expected {
		return errors.New("finalization requires the expected valid snapshot")
	}
	idSnapshot := v.SnapshotID
	if _, e = d.change(ctx, j, "destroy", "snapshot_destroy", "--id", idSnapshot); e != nil {
		return e
	}
	t, e := d.tracking(ctx)
	if e != nil {
		return e
	}
	if t != v.Tracking {
		return errors.New("tracker changed while releasing snapshot")
	}
	image, e := d.snapshot(ctx)
	if e != nil {
		return e
	}
	if image != "" {
		return errors.New("snapshot image remains after release")
	}
	if e = d.collect(ctx, idSnapshot, false); e != nil {
		return e
	}
	v.Current.Mode = "incremental"
	v.Current.SnapshotDevice = ""
	v.SnapshotID = ""
	v.Pending = ""
	return d.save(j)
}
func (d *Driver) Changes(ctx context.Context, id string, expected cbt.Generation) ([]cbt.Extent, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, unlock, e := d.begin(ctx, id)
	if e != nil {
		return nil, 0, e
	}
	defer unlock()
	v, e := d.checked(ctx, j)
	if e != nil {
		return nil, 0, e
	}
	if expected.Mode != "snapshot" || v.Current != expected || v.Previous == 0 || v.Previous+1 != v.Current.Sequence {
		return nil, 0, errors.New("incremental bitmap requires a contiguous previous snapshot")
	}
	f, e := os.CreateTemp(d.cfg.StateDir, ".bitmap-*")
	if e != nil {
		return nil, 0, errors.New("cannot prepare bitmap")
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Close(); e != nil {
		return nil, 0, e
	}
	if _, e = d.command(ctx, "readcbtmap", "--device", d.cfg.Volume.Device, "--file", name); e != nil {
		return nil, 0, e
	}
	st, e := os.Lstat(name)
	if e != nil || !st.Mode().IsRegular() || st.Size() != v.Tracking.Count || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
		return nil, 0, errors.New("invalid exported bitmap size or type")
	}
	f, e = os.Open(name)
	if e != nil {
		return nil, 0, errors.New("cannot read exported bitmap")
	}
	defer f.Close()
	ranges, e := decodeMap(ctx, f, v.Tracking, v.Previous)
	if e != nil {
		return nil, 0, e
	}
	if _, e = d.checked(ctx, j); e != nil {
		return nil, 0, e
	}
	return ranges, v.Tracking.Count, nil
}
func decodeMap(ctx context.Context, r io.Reader, t tracking, previous uint64) ([]cbt.Extent, error) {
	if previous == 0 || previous >= t.Sequence || t.Sequence > 255 || t.Count <= 0 || t.Count > maxMapBytes || t.BlockSize < 512 || t.BlockSize&(t.BlockSize-1) != 0 || t.Size <= 0 || t.Count != (t.Size+t.BlockSize-1)/t.BlockSize {
		return nil, errors.New("invalid bitmap generation or geometry")
	}
	result := []cbt.Extent{}
	buffer := make([]byte, 32768)
	for first := int64(0); first < t.Count; {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		n := int64(len(buffer))
		if n > t.Count-first {
			n = t.Count - first
		}
		if _, e := io.ReadFull(r, buffer[:n]); e != nil {
			return nil, errors.New("truncated exported bitmap")
		}
		for i, b := range buffer[:n] {
			if uint64(b) > t.Sequence {
				return nil, errors.New("bitmap contains a future epoch")
			}
			if uint64(b) <= previous {
				continue
			}
			offset := (first + int64(i)) * t.BlockSize
			length := t.BlockSize
			if length > t.Size-offset {
				length = t.Size - offset
			}
			last := len(result) - 1
			if last >= 0 && result[last].Offset+result[last].Length == offset {
				result[last].Length += length
			} else {
				if len(result) >= 1<<20 {
					return nil, errors.New("bitmap fragmentation exceeds supported bounds")
				}
				result = append(result, cbt.Extent{Offset: offset, Length: length})
			}
		}
		first += n
	}
	var extra [1]byte
	if n, e := r.Read(extra[:]); n != 0 || e != io.EOF {
		return nil, errors.New("bitmap contains trailing data")
	}
	return result, nil
}

// String deliberately excludes private paths and any executable diagnostics.
func (d *Driver) String() string { return fmt.Sprintf("blksnap provider for %s", d.cfg.Volume.ID) }
