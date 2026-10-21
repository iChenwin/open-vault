package blksnap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ichenwin/open-vault/internal/cbt"
)

type fakeKernel struct {
	d        *Driver
	t        *testing.T
	attached bool
	sequence uint64
	uuid     string
	snapshot string
	image    string
	size     int64
	block    int64
	corrupt  bool
	fail     string
	bitmap   []byte
	commands []string
	created  int
}

func fixture(t *testing.T) (*Driver, *fakeKernel) {
	t.Helper()
	// Production forbids writable ancestors, including shared /tmp. Use the
	// current user's private cache so Linux tests exercise the actual path gate.
	cache, e := os.UserCacheDir()
	if e != nil {
		t.Fatal(e)
	}
	base := filepath.Join(cache, "open-vault-blksnap-test")
	if e = os.MkdirAll(base, 0700); e != nil {
		t.Fatal(e)
	}
	dir, e := os.MkdirTemp(base, "case-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	state := filepath.Join(dir, "state")
	source := filepath.Join(dir, "source")
	cow := filepath.Join(dir, "cow")
	binary := filepath.Join(dir, "blksnap")
	device := filepath.Join(dir, "device")
	for _, p := range []string{state, source, cow} {
		if e = os.Mkdir(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	for _, p := range []string{binary, device} {
		if e = os.WriteFile(p, []byte("fixture"), 0700); e != nil {
			t.Fatal(e)
		}
	}
	d, e := New(Config{Binary: binary, StateDir: state, Volume: cbt.Volume{ID: "data", Device: device, MountPoint: source, COWDir: cow}, DifferenceLimit: "1G"})
	if e != nil {
		t.Fatal(e)
	}
	f := &fakeKernel{d: d, t: t, uuid: "01234567-89ab-cdef-0123-456789abcdef", size: 8192, block: 512, bitmap: make([]byte, 16)}
	d.validate = func(cbt.Volume) error { return nil }
	d.platform = func() (string, bool) { return "6.8.0-147-generic", true }
	d.boot = func() (string, error) { return "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil }
	d.size = func(string) (int64, error) { return f.size, nil }
	d.command = f.run
	return d, f
}
func (f *fakeKernel) run(ctx context.Context, args ...string) ([]byte, error) {
	f.t.Helper()
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	op := args[0]
	f.commands = append(f.commands, op)
	arg := func(key string) string {
		for i := 1; i+1 < len(args); i++ {
			if args[i] == key {
				return args[i+1]
			}
		}
		return ""
	}
	mutating := map[string]string{"attach": "attach", "snapshot_create": "create", "snapshot_take": "take", "snapshot_destroy": "destroy"}
	if phase, ok := mutating[op]; ok {
		b, e := os.ReadFile(f.d.statePath())
		if e != nil {
			f.t.Fatal("mutation without durable journal", e)
		}
		var j journal
		if e = json.Unmarshal(b, &j); e != nil || j.Entry == nil || j.Entry.Pending != phase {
			f.t.Fatalf("mutation %s lacks durable pending phase: %s", op, b)
		}
		if op == "snapshot_take" && j.Entry.SnapshotID != f.snapshot {
			f.t.Fatal("snapshot identity not persisted before taking")
		}
	}
	if f.fail == op {
		return nil, errors.New("simulated uncertain command")
	}
	switch op {
	case "version":
		return []byte(ModuleVersion + "\n"), nil
	case "attach":
		if f.attached {
			return []byte("Already was attached\n"), nil
		}
		f.attached = true
		return []byte("Attached successfully\n"), nil
	case "cbtinfo":
		if !f.attached {
			return nil, errors.New("no tracker")
		}
		return []byte(fmt.Sprintf("block_size=%d\ndevice_capacity=%d\nblock_count=%d\ngeneration_id=%s\nchanges_number=%d\n", f.block, f.size, (f.size+f.block-1)/f.block, f.uuid, f.sequence)), nil
	case "snapshot_create":
		if f.snapshot != "" {
			return nil, errors.New("snapshot exists")
		}
		f.created++
		f.snapshot = fmt.Sprintf("11111111-2222-3333-4444-%012d", f.created)
		return []byte(f.snapshot + "\n"), nil
	case "snapshot_take":
		if arg("--id") != f.snapshot {
			return nil, errors.New("wrong snapshot")
		}
		f.sequence++
		f.image = "/dev/vbsnap-8-16"
		return nil, nil
	case "snapshot_info":
		code := 0
		if f.corrupt {
			code = -5
		}
		image := f.image
		if image == "" {
			image = "/dev/"
		}
		return []byte(fmt.Sprintf("error_code=%d\nimage=%s\n", code, image)), nil
	case "snapshot_collect":
		if f.snapshot == "" {
			return []byte("\n"), nil
		}
		return []byte(f.snapshot + " \n\n"), nil
	case "snapshot_destroy":
		if arg("--id") != f.snapshot {
			return nil, errors.New("wrong snapshot")
		}
		f.snapshot = ""
		f.image = ""
		return nil, nil
	case "readcbtmap":
		return nil, os.WriteFile(arg("--file"), f.bitmap, 0600)
	default:
		return nil, errors.New("unexpected command")
	}
}
func snapshot2(t *testing.T, d *Driver) cbt.Generation {
	t.Helper()
	ctx := context.Background()
	first, e := d.Setup(ctx, "data", "baseline")
	if e != nil {
		t.Fatal(e)
	}
	if e = d.ToIncremental(ctx, "data", first); e != nil {
		t.Fatal(e)
	}
	tr, e := d.ToSnapshot(ctx, "data", "second")
	if e != nil {
		t.Fatal(e)
	}
	return tr.Current
}

func TestLifecycleAndFrozenBitmap(t *testing.T) {
	d, f := fixture(t)
	ctx := context.Background()
	if _, e := d.Current(ctx, "data"); !errors.Is(e, cbt.ErrNoGeneration) {
		t.Fatalf("new state: %v", e)
	}
	first, e := d.Setup(ctx, "data", "baseline")
	if e != nil {
		t.Fatal(e)
	}
	if first.Sequence != 1 || first.Mode != "snapshot" || first.Size != 8192 || first.UUID != "0123456789abcdef0123456789abcdef" || first.COWFile != "" {
		t.Fatalf("first: %+v", first)
	}
	if e = d.Verify(ctx, "data", first); e != nil {
		t.Fatal(e)
	}
	if _, _, e = d.Changes(ctx, "data", first); e == nil {
		t.Fatal("baseline accepted as an increment")
	}
	if e = d.ToIncremental(ctx, "data", first); e != nil {
		t.Fatal(e)
	}
	tr, e := d.Current(ctx, "data")
	if e != nil || tr.Current.Mode != "incremental" || !f.attached {
		t.Fatalf("tracker not retained: %+v %v", tr, e)
	}
	tr, e = d.ToSnapshot(ctx, "data", "second")
	if e != nil {
		t.Fatal(e)
	}
	if tr.PreviousSequence != 1 || tr.PreviousCOW != "" || tr.Current.Sequence != 2 {
		t.Fatalf("transition: %+v", tr)
	}
	f.bitmap = []byte{0, 1, 2, 2, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}
	changes, n, e := d.Changes(ctx, "data", tr.Current)
	if e != nil {
		t.Fatal(e)
	}
	want := []cbt.Extent{{Offset: 1024, Length: 1024}, {Offset: 2560, Length: 512}, {Offset: 7680, Length: 512}}
	if n != 16 || !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes %v/%d want %v", changes, n, want)
	}
	// Reopening the provider preserves a valid active snapshot for ConfirmHead
	// recovery. Finalization remains explicit and uses the expected generation.
	d2, e := New(d.cfg)
	if e != nil {
		t.Fatal(e)
	}
	d2.validate = d.validate
	d2.platform = d.platform
	d2.size = d.size
	d2.boot = d.boot
	d2.command = f.run
	if e = d2.Verify(ctx, "data", tr.Current); e != nil {
		t.Fatal(e)
	}
	if e = d2.ToIncremental(ctx, "data", tr.Current); e != nil {
		t.Fatal(e)
	}
	if !f.attached || f.snapshot != "" {
		t.Fatal("finalize destroyed tracker or retained snapshot")
	}
	if e = d2.ToIncremental(ctx, "data", tr.Current); e == nil {
		t.Fatal("stale snapshot finalized twice")
	}
}

func TestUncertainMutationsRemainPending(t *testing.T) {
	for _, op := range []string{"attach", "snapshot_create", "snapshot_take", "snapshot_destroy"} {
		t.Run(op, func(t *testing.T) {
			d, f := fixture(t)
			ctx := context.Background()
			var g cbt.Generation
			var e error
			if op == "snapshot_destroy" {
				g, e = d.Setup(ctx, "data", "full")
				if e != nil {
					t.Fatal(e)
				}
			}
			f.fail = op
			if op == "snapshot_destroy" {
				e = d.ToIncremental(ctx, "data", g)
			} else {
				_, e = d.Setup(ctx, "data", "full")
			}
			if e == nil {
				t.Fatal("uncertain command succeeded")
			}
			f.fail = ""
			before := len(f.commands)
			if _, e = d.Current(ctx, "data"); e == nil || !strings.Contains(e.Error(), "pending") {
				t.Fatalf("pending operation resumed: %v", e)
			}
			if len(f.commands) != before {
				t.Fatal("pending recovery executed another kernel command")
			}
		})
	}
}

func TestRejectInconsistentLiveState(t *testing.T) {
	for _, kind := range []string{"uuid", "sequence", "size", "block", "boot", "snapshot", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			d, f := fixture(t)
			g := snapshot2(t, d)
			switch kind {
			case "uuid":
				f.uuid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			case "sequence":
				f.sequence++
			case "size":
				f.size += 512
			case "block":
				f.block *= 2
			case "boot":
				d.boot = func() (string, error) { return "new-boot", nil }
			case "snapshot":
				f.snapshot = "99999999-2222-3333-4444-000000000001"
			case "corrupt":
				f.corrupt = true
			}
			before := strings.Join(f.commands, ",")
			if e := d.Verify(context.Background(), "data", g); e == nil {
				t.Fatal("changed live generation verified")
			}
			if e := d.ToIncremental(context.Background(), "data", g); e == nil {
				t.Fatal("invalid snapshot finalized")
			}
			if strings.Count(strings.Join(f.commands, ","), "snapshot_destroy") != strings.Count(before, "snapshot_destroy") {
				t.Fatal("invalid snapshot was destroyed")
			}
		})
	}
}

func TestBitmapRejectsMalformedAndChangedSnapshot(t *testing.T) {
	for _, kind := range []string{"short", "long", "future", "corrupt-during-read", "counter-during-read"} {
		t.Run(kind, func(t *testing.T) {
			d, f := fixture(t)
			g := snapshot2(t, d)
			f.bitmap = make([]byte, 16)
			switch kind {
			case "short":
				f.bitmap = f.bitmap[:15]
			case "long":
				f.bitmap = make([]byte, 17)
			case "future":
				f.bitmap[3] = 3
			}
			original := d.command
			d.command = func(ctx context.Context, args ...string) ([]byte, error) {
				b, e := original(ctx, args...)
				if args[0] == "readcbtmap" {
					if kind == "corrupt-during-read" {
						f.corrupt = true
					}
					if kind == "counter-during-read" {
						f.sequence++
					}
				}
				return b, e
			}
			if _, _, e := d.Changes(context.Background(), "data", g); e == nil {
				t.Fatal("invalid or unstable bitmap accepted")
			}
		})
	}
}

func TestDecodeMapCoalescesAndCropsTail(t *testing.T) {
	tt := tracking{Sequence: 4, BlockSize: 1024, Count: 4, Size: 3584}
	got, e := decodeMap(context.Background(), bytes.NewReader([]byte{1, 4, 4, 4}), tt, 3)
	if e != nil || !reflect.DeepEqual(got, []cbt.Extent{{Offset: 1024, Length: 2560}}) {
		t.Fatalf("tail extent: %v %v", got, e)
	}
	for _, data := range [][]byte{{0, 4, 4}, {0, 4, 4, 4, 0}, {0, 5, 0, 0}} {
		if _, e = decodeMap(context.Background(), bytes.NewReader(data), tt, 3); e == nil {
			t.Fatal("invalid bitmap accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = decodeMap(ctx, bytes.NewReader([]byte{4, 4, 4, 4}), tt, 3); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation: %v", e)
	}
}

func TestStrictMetadataAndRollover(t *testing.T) {
	d, f := fixture(t)
	g := snapshot2(t, d)
	if e := d.ToIncremental(context.Background(), "data", g); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(d.statePath())
	if e != nil {
		t.Fatal(e)
	}
	var j journal
	if e = json.Unmarshal(b, &j); e != nil {
		t.Fatal(e)
	}
	j.Entry.Current.Sequence = 255
	j.Entry.Tracking.Sequence = 255
	j.Entry.Previous = 254
	f.sequence = 255
	if e = d.save(&j); e != nil {
		t.Fatal(e)
	}
	before := len(f.commands)
	if _, e = d.ToSnapshot(context.Background(), "data", "rollover"); e == nil {
		t.Fatal("rollover allowed")
	}
	for _, cmd := range f.commands[before:] {
		if cmd == "snapshot_create" || cmd == "snapshot_take" {
			t.Fatal("rollover mutated kernel state")
		}
	}
	for _, bad := range []string{"block_size=512\nblock_size=512\n", "block_size=512\nextra=x\n", "block_size=\n"} {
		if _, e = parseFields([]byte(bad), "block_size"); e == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
}

func TestUnknownAttachVersionAndTrustedPaths(t *testing.T) {
	d, f := fixture(t)
	f.attached = true
	if _, e := d.Setup(context.Background(), "data", "full"); e == nil {
		t.Fatal("adopted unknown tracker")
	}
	d, f = fixture(t)
	original := d.command
	d.command = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "version" {
			return []byte("7.0.0.1\n"), nil
		}
		return original(ctx, args...)
	}
	if _, e := d.Setup(context.Background(), "data", "full"); e == nil || f.attached {
		t.Fatal("unrecognized driver version attached tracker")
	}
	c := d.cfg
	c.StateDir = c.Volume.COWDir
	if _, e := New(c); e == nil {
		t.Fatal("state and COW overlap accepted")
	}
	c = d.cfg
	c.DifferenceLimit = "1G;arbitrary-command"
	if _, e := New(c); e == nil {
		t.Fatal("nonliteral storage limit accepted")
	}
	if runtime.GOOS == "linux" {
		if e := os.Chmod(d.cfg.Binary, 0777); e != nil {
			t.Fatal(e)
		}
		if _, e := New(d.cfg); e == nil {
			t.Fatal("writable privileged binary accepted")
		}
	}
}

func TestDiagnoseNeverCreatesTrackingState(t *testing.T) {
	d, f := fixture(t)
	d.validate = func(cbt.Volume) error { return errors.New("unapproved source mount") }
	r, err := d.Diagnose(context.Background())
	if err != nil || r.Provider != "blksnap" || !r.Supported || !r.DriverLoaded || len(r.Devices) != 0 || len(r.Problems) != 1 {
		t.Fatalf("diagnosis: %+v %v", r, err)
	}
	if f.attached || !reflect.DeepEqual(f.commands, []string{"version"}) {
		t.Fatal("diagnosis mutated or queried an unattached tracker")
	}
	if _, err = os.Lstat(d.statePath()); !os.IsNotExist(err) {
		t.Fatal("diagnosis created journal")
	}
	if _, err = os.Lstat(filepath.Join(d.cfg.StateDir, ".operation-lock")); !os.IsNotExist(err) {
		t.Fatal("diagnosis created a lock")
	}
}

func TestUnexpectedCommandRepliesFailClosed(t *testing.T) {
	for _, kind := range []string{"create-id", "extra-field", "bad-number", "wrong-image", "duplicate-id"} {
		t.Run(kind, func(t *testing.T) {
			d, f := fixture(t)
			original := d.command
			d.command = func(ctx context.Context, args ...string) ([]byte, error) {
				b, err := original(ctx, args...)
				switch {
				case kind == "create-id" && args[0] == "snapshot_create":
					return []byte("unexpected status text"), nil
				case kind == "extra-field" && args[0] == "cbtinfo":
					return append(b, []byte("unknown=1\n")...), nil
				case kind == "bad-number" && args[0] == "cbtinfo":
					return bytes.Replace(b, []byte("block_size=512"), []byte("block_size=+512"), 1), nil
				case kind == "wrong-image" && args[0] == "snapshot_info":
					return []byte("error_code=0\nimage=/dev/sda\n"), nil
				case kind == "duplicate-id" && args[0] == "snapshot_collect":
					return []byte(f.snapshot + " " + f.snapshot), nil
				}
				return b, err
			}
			if _, err := d.Setup(context.Background(), "data", "baseline"); err == nil {
				t.Fatal("unexpected response accepted")
			}
			if _, err := d.Current(context.Background(), "data"); err == nil || !strings.Contains(err.Error(), "pending") {
				t.Fatalf("uncertain generation automatically resumed: %v", err)
			}
		})
	}
}

func TestCommandSubprocessHelper(t *testing.T) {
	args := os.Args
	if len(args) < 2 {
		return
	}
	mode := args[len(args)-1]
	switch mode {
	case "helper-stdout":
		for i := 0; i < 5; i++ {
			_, _ = os.Stdout.Write(bytes.Repeat([]byte{'x'}, 1<<20))
		}
		os.Exit(0)
	case "helper-stderr":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte{'s'}, 65<<10))
		os.Exit(0)
	case "helper-timeout":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
}

func TestRealCommandOutputAndCancellationBounds(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{cfg: Config{Binary: binary}}
	for _, mode := range []string{"helper-stdout", "helper-stderr", "helper-timeout"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if mode == "helper-timeout" {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
		}
		_, err := d.run(ctx, "-test.run=^TestCommandSubprocessHelper$", "--", mode)
		cancel()
		if err == nil {
			t.Fatalf("unbounded command accepted: %s", mode)
		}
	}
}
