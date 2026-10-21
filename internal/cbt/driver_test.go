package cbt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const testUUID = "00112233445566778899aabbccddeeff"

type driverFixture struct {
	d          *Driver
	v          Volume
	info       procInfo
	commands   [][]string
	boot       string
	commandErr error
}

func fixture(t *testing.T) *driverFixture {
	t.Helper()
	base := t.TempDir()
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	v := Volume{ID: "data", Device: filepath.Join(base, "disk"), MountPoint: filepath.Join(base, "source"), Minor: 7}
	v.COWDir = filepath.Join(v.MountPoint, "cows")
	if err := os.MkdirAll(v.COWDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v.Device, []byte("fake device"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &Driver{config: DriverConfig{ProcPath: filepath.Join(base, "proc.json"), StateFile: filepath.Join(base, "journal.json"), Volumes: []Volume{v}}}
	f := &driverFixture{d: d, v: v, info: procInfo{Version: UpstreamVersion, Devices: []DeviceStatus{}}, boot: "boot-one"}
	d.validate = func(Volume) error { return nil }
	d.platform = func() (string, bool) { return "6.8.0-99-generic", true }
	d.bootID = func() (string, error) { return f.boot, nil }
	d.size = func(string) (int64, error) { return 4 << 20, nil }
	d.command = func(ctx context.Context, args ...string) error {
		f.commands = append(f.commands, append([]string{}, args...))
		if f.commandErr != nil {
			return f.commandErr
		}
		var cow string
		state := uint32(3)
		switch args[0] {
		case "setup-snapshot":
			cow = args[2]
			f.info.Devices = []DeviceStatus{{Minor: v.Minor, BlockDevice: v.Device, UUID: testUUID, Sequence: 1, State: &state, Version: 1}}
		case "transition-to-incremental":
			state = 2
			f.info.Devices[0].State = &state
		case "transition-to-snapshot":
			cow = args[1]
			f.info.Devices[0].Sequence++
			f.info.Devices[0].State = &state
		default:
			t.Fatalf("unexpected external command %q", args)
		}
		if cow != "" {
			if err := os.WriteFile(cow, []byte("fake COW file"), 0600); err != nil {
				return err
			}
			rel, err := filepath.Rel(v.MountPoint, cow)
			if err != nil {
				return err
			}
			f.info.Devices[0].COWFile = "/" + filepath.ToSlash(rel)
		}
		f.write(t)
		return nil
	}
	f.write(t)
	return f
}

func (f *driverFixture) write(t *testing.T) {
	t.Helper()
	b, err := json.Marshal(f.info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.d.config.ProcPath, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDriverGenerationsAndCommands(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	if _, err := f.d.Current(ctx, "data"); !errors.Is(err, ErrNoGeneration) {
		t.Fatalf("expected fresh journal: %v", err)
	}
	g, err := f.d.Setup(ctx, "data", "cow_1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Mode != "snapshot" || g.Size != 4<<20 || g.Sequence != 1 || g.UUID != testUUID {
		t.Fatalf("wrong generation: %+v", g)
	}
	if err := f.d.Verify(ctx, "data", g); err != nil {
		t.Fatal(err)
	}
	if err := f.d.ToIncremental(ctx, "data", g); err != nil {
		t.Fatal(err)
	}
	current, err := f.d.Current(ctx, "data")
	if err != nil {
		t.Fatal(err)
	}
	if current.Current.Mode != "incremental" {
		t.Fatal("incremental mode not persisted")
	}
	if err := f.d.Verify(ctx, "data", g); err == nil {
		t.Fatal("accepted stale snapshot generation")
	}
	if err := f.d.Verify(ctx, "data", current.Current); err != nil {
		t.Fatal(err)
	}
	next, err := f.d.ToSnapshot(ctx, "data", "cow_2")
	if err != nil {
		t.Fatal(err)
	}
	if next.Current.Sequence != 2 || next.PreviousSequence != 1 || next.PreviousCOW != g.COWFile {
		t.Fatalf("lost previous bitmap: %+v", next)
	}
	retry, err := f.d.Current(ctx, "data")
	if err != nil || retry != next {
		t.Fatalf("snapshot retry lost transition: %+v %v", retry, err)
	}
	if _, err := f.d.ToSnapshot(ctx, "data", "cow_3"); err == nil {
		t.Fatal("transitioned an active snapshot again")
	}
	want := [][]string{
		{"setup-snapshot", f.v.Device, filepath.Join(f.v.COWDir, "cow_1"), "7"},
		{"transition-to-incremental", "7"},
		{"transition-to-snapshot", filepath.Join(f.v.COWDir, "cow_2"), "7"},
	}
	if !reflect.DeepEqual(f.commands, want) {
		t.Fatalf("unexpected argv: %#v", f.commands)
	}
	if _, err := f.d.Setup(ctx, "data", "fresh"); err == nil {
		t.Fatal("silently reset tracking history")
	}
}

func TestDriverFailClosed(t *testing.T) {
	for _, test := range []string{"pending", "boot", "uuid", "sequence", "cow", "dormant", "unverified", "error_zero", "driver_version", "source", "size"} {
		t.Run(test, func(t *testing.T) {
			f := fixture(t)
			ctx := context.Background()
			g, err := f.d.Setup(ctx, "data", "cow_1")
			if err != nil {
				t.Fatal(err)
			}
			s := &f.info.Devices[0]
			switch test {
			case "pending":
				f.commandErr = errors.New("simulated timeout")
				if err := f.d.ToIncremental(ctx, "data", g); err == nil {
					t.Fatal("expected command failure")
				}
			case "boot":
				f.boot = "boot-two"
			case "uuid":
				s.UUID = strings.Repeat("a", 32)
			case "sequence":
				s.Sequence++
			case "cow":
				s.COWFile = "/../outside"
			case "dormant":
				state := uint32(1)
				s.State = &state
			case "unverified":
				state := uint32(5)
				s.State = &state
			case "error_zero":
				code := 0
				s.Error = &code
			case "driver_version":
				f.info.Version = "0.12.2"
			case "source":
				s.BlockDevice = filepath.Join(t.TempDir(), "different")
			case "size":
				f.d.size = func(string) (int64, error) { return 8 << 20, nil }
			}
			f.write(t)
			if _, err := f.d.Current(ctx, "data"); err == nil || errors.Is(err, ErrNoGeneration) {
				t.Fatalf("unsafe continuity result: %v", err)
			}
			if _, err := f.d.Setup(ctx, "data", "new_baseline"); err == nil {
				t.Fatal("reinitialized failed tracking")
			}
		})
	}
}

func TestInterruptedSetupIsPending(t *testing.T) {
	f := fixture(t)
	f.commandErr = errors.New("command interrupted")
	if _, err := f.d.Setup(context.Background(), "data", "cow_1"); err == nil {
		t.Fatal("expected failed setup")
	}
	f.commandErr = nil
	if _, err := f.d.Current(context.Background(), "data"); err == nil || errors.Is(err, ErrNoGeneration) {
		t.Fatal("treated pending setup as fresh state")
	}
	if len(f.commands) != 1 {
		t.Fatal("unexpected retry")
	}
}

func TestUnsupportedPlatformAndNames(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	for _, name := range []string{"../escape", "a/b", "a\\b", "--option;touch", "", strings.Repeat("x", 81)} {
		if _, err := f.d.Setup(ctx, "data", name); err == nil {
			t.Errorf("accepted COW name %q", name)
		}
	}
	if len(f.commands) != 0 {
		t.Fatal("invalid request executed external command")
	}
	f.d.platform = func() (string, bool) { return "6.18.0-wsl", false }
	if _, err := f.d.Setup(ctx, "data", "cow"); err == nil {
		t.Fatal("enabled unsupported kernel")
	}
	r, err := f.d.Diagnose(ctx)
	if err != nil || r.Supported || !r.DriverLoaded || len(r.Problems) == 0 {
		t.Fatalf("incorrect diagnostic: %+v %v", r, err)
	}
	if _, err := f.d.Setup(ctx, "unknown", "cow"); err == nil {
		t.Fatal("accepted non-allowlisted volume")
	}
}

func TestNoReuseOfCowAndUnexpectedTracking(t *testing.T) {
	f := fixture(t)
	if err := os.WriteFile(filepath.Join(f.v.COWDir, "exists"), []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Setup(context.Background(), "data", "exists"); err == nil {
		t.Fatal("overwrote existing COW file")
	}
	state := uint32(2)
	f.info.Devices = []DeviceStatus{{Minor: f.v.Minor, State: &state, BlockDevice: f.v.Device}}
	f.write(t)
	if _, err := f.d.Setup(context.Background(), "data", "new"); err == nil {
		t.Fatal("took over externally tracked minor")
	}
	if len(f.commands) != 0 {
		t.Fatal("unsafe command executed")
	}
}

func TestJournalLockAndCorruption(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	if err := os.Mkdir(f.d.config.StateFile+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Setup(ctx, "data", "cow"); err == nil {
		t.Fatal("ignored stale process lock")
	}
	if err := os.Remove(f.d.config.StateFile + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.d.config.StateFile, []byte(`{"version":1,"volumes":{},"unexpected":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Setup(ctx, "data", "cow"); err == nil {
		t.Fatal("accepted corrupt journal")
	}
}

func TestMountInfoParsing(t *testing.T) {
	data := []byte("20 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n21 1 8:16 / /mnt/data\\040drive rw - xfs /dev/sdb rw\n")
	mounts, err := parseMounts(data)
	if err != nil || len(mounts) != 2 || mounts[1].mountPoint != "/mnt/data drive" || mounts[1].filesystem != "xfs" {
		t.Fatalf("bad mount parse: %+v %v", mounts, err)
	}
	if _, err := parseMounts([]byte("broken")); err == nil {
		t.Fatal("accepted bad mountinfo")
	}
	if _, err := mountUnescape("/bad\\999"); err == nil {
		t.Fatal("accepted invalid escape")
	}
}

func TestDiscardMustBeDisabled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux sysfs names use colons")
	}
	root := t.TempDir()
	dev := filepath.Join(root, "dev", "block", "8:16")
	if err := os.MkdirAll(filepath.Join(dev, "queue"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dev, "slaves"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dev, "queue", "discard_max_bytes")
	if err := os.WriteFile(path, []byte("1048576\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := discardDisabled(root, "8:16"); err == nil {
		t.Fatal("allowed discarded blocks to escape tracking")
	}
	if err := os.WriteFile(path, []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := discardDisabled(root, "8:16"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := discardDisabled(root, "8:16"); err == nil {
		t.Fatal("accepted missing queue information")
	}
}

func cowBytes(t *testing.T, blocks int, changed ...int) []byte {
	t.Helper()
	b := make([]byte, 4096+blocks*8)
	binary.LittleEndian.PutUint32(b[0:4], 4776)
	binary.LittleEndian.PutUint32(b[4:8], 3)
	binary.LittleEndian.PutUint64(b[24:32], 9)
	uuid, _ := hex.DecodeString(testUUID)
	copy(b[32:48], uuid)
	binary.LittleEndian.PutUint64(b[48:56], 1)
	binary.LittleEndian.PutUint64(b[56:64], uint64(len(changed)))
	for _, i := range changed {
		binary.LittleEndian.PutUint64(b[4096+i*8:], 1)
	}
	return b
}

func TestReadChanges(t *testing.T) {
	b := cowBytes(t, 6, 0, 1, 4, 5)
	extents, err := ReadChanges(bytes.NewReader(b), 6*4096, testUUID, 9)
	want := []Extent{{0, 8192}, {4 * 4096, 8192}}
	if err != nil || !reflect.DeepEqual(extents, want) {
		t.Fatalf("wrong extents: %+v %v", extents, err)
	}
	b = cowBytes(t, 2, 1)
	extents, err = ReadChanges(bytes.NewReader(b), 4096+512, testUUID, 9)
	if err != nil || !reflect.DeepEqual(extents, []Extent{{4096, 512}}) {
		t.Fatalf("tail block not cropped: %+v %v", extents, err)
	}
	b = cowBytes(t, 3)
	extents, err = ReadChanges(bytes.NewReader(b), 3*4096, testUUID, 9)
	if err != nil || len(extents) != 0 {
		t.Fatalf("empty bitmap: %+v %v", extents, err)
	}
}

func TestReadChangesRejectsInvalidOrStaleIndex(t *testing.T) {
	for _, test := range []string{"header", "index", "magic", "dirty", "snapshot", "flags", "uuid", "sequence", "version", "count"} {
		t.Run(test, func(t *testing.T) {
			b := cowBytes(t, 4, 0, 2)
			switch test {
			case "header":
				b = b[:60]
			case "index":
				b = b[:len(b)-1]
			case "magic":
				b[0] = 0
			case "dirty":
				binary.LittleEndian.PutUint32(b[4:8], 2)
			case "snapshot":
				binary.LittleEndian.PutUint32(b[4:8], 1)
			case "flags":
				binary.LittleEndian.PutUint32(b[4:8], 11)
			case "uuid":
				b[32]++
			case "sequence":
				b[24]++
			case "version":
				b[48] = 2
			case "count":
				b[56] = 3
			}
			if _, err := ReadChanges(bytes.NewReader(b), 4*4096, testUUID, 9); err == nil {
				t.Fatal("accepted unsafe change index")
			}
		})
	}
	for _, size := range []int64{-1, 0, 1, 4095, 65 << 40} {
		if _, err := ReadChanges(bytes.NewReader(cowBytes(t, 4)), size, testUUID, 9); err == nil {
			t.Errorf("accepted invalid size %d", size)
		}
	}
}
