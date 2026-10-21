// Package cbt integrates with an independently installed Datto block driver.
// No kernel or libdattobd implementation is linked or vendored here.
package cbt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const UpstreamCommit = "229ca998ce570ab1b74172d55dda62783d5b2d8d"
const UpstreamVersion = "0.11.10"

// ErrNoGeneration only means this journal has never tracked the requested
// volume. Other failures must never trigger automatic setup or baseline reuse.
var ErrNoGeneration = errors.New("no durable tracking generation")

type DriverConfig struct {
	DBDCtl        string   `json:"dbdctl"`
	ProcPath      string   `json:"proc_path,omitempty"`
	MountInfoPath string   `json:"mount_info_path,omitempty"`
	StateFile     string   `json:"state_file"`
	Volumes       []Volume `json:"volumes"`
}

type Volume struct {
	ID         string `json:"id"`
	Device     string `json:"device"`
	MountPoint string `json:"mount_point"`
	COWDir     string `json:"cow_dir"`
	Minor      uint32 `json:"minor"`
}

type Generation struct {
	VolumeID       string `json:"volume_id"`
	UUID           string `json:"uuid"`
	COWFile        string `json:"cow_file"`
	SnapshotDevice string `json:"snapshot_device"`
	Sequence       uint64 `json:"sequence"`
	Minor          uint32 `json:"minor"`
	Mode           string `json:"mode"`
	Size           int64  `json:"size"`
}

type Transition struct {
	Current          Generation `json:"current"`
	PreviousCOW      string     `json:"previous_cow,omitempty"`
	PreviousSequence uint64     `json:"previous_sequence,omitempty"`
}

type DeviceStatus struct {
	Minor         uint32  `json:"minor"`
	COWFile       string  `json:"cow_file"`
	BlockDevice   string  `json:"block_device"`
	Sequence      uint64  `json:"seq_id"`
	UUID          string  `json:"uuid"`
	State         *uint32 `json:"state"`
	Error         *int    `json:"error,omitempty"`
	Version       uint64  `json:"version"`
	ChangedBlocks uint64  `json:"nr_changed_blocks"`
}

type Report struct {
	Supported     bool           `json:"supported"`
	Kernel        string         `json:"kernel"`
	Architecture  string         `json:"architecture"`
	DriverLoaded  bool           `json:"driver_loaded"`
	DriverVersion string         `json:"driver_version,omitempty"`
	Devices       []DeviceStatus `json:"devices"`
	Problems      []string       `json:"problems"`
}

type procInfo struct {
	Version string         `json:"version"`
	Devices []DeviceStatus `json:"devices"`
}

type journalEntry struct {
	Transition
	Pending bool   `json:"pending"`
	BootID  string `json:"boot_id"`
}
type journal struct {
	Version int                     `json:"version"`
	Volumes map[string]journalEntry `json:"volumes"`
}

type Driver struct {
	mu     sync.Mutex
	config DriverConfig
	// Test seams are private: production callers cannot bypass the platform,
	// block-device, discard or boot-continuity checks through JSON configuration.
	validate func(Volume) error
	command  func(context.Context, ...string) error
	bootID   func() (string, error)
	platform func() (string, bool)
	size     func(string) (int64, error)
}

var safeName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)
var kernelGA = regexp.MustCompile(`^6\.8\.0-[0-9]+-generic$`)
var uuidPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func NewDriver(c DriverConfig) (*Driver, error) {
	if c.DBDCtl == "" {
		c.DBDCtl = "/usr/sbin/dbdctl"
	}
	if c.ProcPath == "" {
		c.ProcPath = "/proc/datto-info"
	}
	if c.MountInfoPath == "" {
		c.MountInfoPath = "/proc/self/mountinfo"
	}
	for _, p := range []string{c.DBDCtl, c.ProcPath, c.MountInfoPath, c.StateFile} {
		if !filepath.IsAbs(p) {
			return nil, errors.New("driver configuration paths must be absolute")
		}
	}
	if len(c.Volumes) == 0 {
		return nil, errors.New("at least one approved data volume is required")
	}
	ids, minors, devices := map[string]bool{}, map[uint32]bool{}, map[string]bool{}
	c.Volumes = append([]Volume(nil), c.Volumes...)
	for i, v := range c.Volumes {
		if !safeName.MatchString(v.ID) || ids[v.ID] || minors[v.Minor] || v.Minor > 255 {
			return nil, errors.New("invalid or duplicate volume ID/minor")
		}
		ids[v.ID], minors[v.Minor] = true, true
		for _, p := range []string{v.Device, v.MountPoint, v.COWDir} {
			if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\r\n\"\\") {
				return nil, errors.New("volume paths must be absolute and contain no control characters, quotes or backslashes")
			}
		}
		var err error
		if v.Device, err = filepath.EvalSymlinks(v.Device); err != nil {
			return nil, errors.New("approved block device is unavailable")
		}
		if v.MountPoint, err = filepath.EvalSymlinks(v.MountPoint); err != nil {
			return nil, errors.New("approved mount point is unavailable")
		}
		if v.COWDir, err = filepath.EvalSymlinks(v.COWDir); err != nil {
			return nil, errors.New("approved COW directory must already exist")
		}
		if devices[v.Device] || !pathWithin(v.MountPoint, v.COWDir) {
			return nil, errors.New("duplicate device or COW directory outside source mount")
		}
		if v.MountPoint == string(filepath.Separator) || pathWithin("/boot", v.MountPoint) {
			return nil, errors.New("system volumes cannot be tracked")
		}
		devices[v.Device] = true
		c.Volumes[i] = v
	}
	if err := os.MkdirAll(filepath.Dir(c.StateFile), 0700); err != nil {
		return nil, errors.New("cannot prepare driver state directory")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(c.StateFile))
	if err != nil {
		return nil, errors.New("invalid driver state directory")
	}
	c.StateFile = filepath.Join(parent, filepath.Base(c.StateFile))
	for _, v := range c.Volumes {
		if pathWithin(v.MountPoint, c.StateFile) {
			return nil, errors.New("driver journal must be outside tracked volumes")
		}
	}
	d := &Driver{config: c}
	d.validate = d.validateVolume
	d.command = d.run
	d.platform = hostPlatform
	d.size = deviceSize
	d.bootID = func() (string, error) {
		b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil || len(strings.TrimSpace(string(b))) != 36 {
			return "", errors.New("cannot establish boot continuity")
		}
		return strings.TrimSpace(string(b)), nil
	}
	return d, nil
}

func deviceSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, errors.New("cannot open approved block device")
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil || size <= 0 || size%512 != 0 {
		return 0, errors.New("cannot determine block device size")
	}
	return size, nil
}

func hostPlatform() (string, bool) {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	kernel := strings.TrimSpace(string(b))
	if err != nil {
		return kernel, false
	}
	osRelease, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return kernel, false
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(osRelease), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[k] = strings.Trim(v, "\"")
		}
	}
	return kernel, runtime.GOOS == "linux" && runtime.GOARCH == "amd64" && values["ID"] == "ubuntu" && values["VERSION_ID"] == "24.04" && kernelGA.MatchString(kernel)
}

func pathWithin(root, path string) bool {
	r, err := filepath.Rel(root, path)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("driver metadata exceeds size limit")
	}
	return b, nil
}

func (d *Driver) readProc() (procInfo, error) {
	var info procInfo
	b, err := readLimited(d.config.ProcPath, 4<<20)
	if err != nil {
		return info, errors.New("dattobd status unavailable")
	}
	if err = json.Unmarshal(b, &info); err != nil || info.Version == "" || info.Devices == nil {
		return info, errors.New("invalid dattobd status")
	}
	seen := map[uint32]bool{}
	for _, dev := range info.Devices {
		if seen[dev.Minor] || dev.State == nil {
			return info, errors.New("ambiguous dattobd status")
		}
		seen[dev.Minor] = true
	}
	return info, nil
}

func (d *Driver) Diagnose(ctx context.Context) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	kernel, supported := d.platform()
	r := Report{Supported: supported, Kernel: kernel, Architecture: runtime.GOARCH, Devices: []DeviceStatus{}, Problems: []string{}}
	if !supported {
		r.Problems = append(r.Problems, "experimental tracking requires Ubuntu 24.04 GA 6.8 generic kernel on amd64")
	}
	info, err := d.readProc()
	if err != nil {
		r.Problems = append(r.Problems, err.Error())
		return r, nil
	}
	r.DriverLoaded = true
	r.DriverVersion = info.Version
	r.Devices = info.Devices
	if info.Version != UpstreamVersion {
		r.Problems = append(r.Problems, "unvalidated dattobd version; expected "+UpstreamVersion)
	}
	for _, v := range d.config.Volumes {
		if err := d.validate(v); err != nil {
			r.Problems = append(r.Problems, v.ID+": "+err.Error())
		}
	}
	return r, nil
}

type mountEntry struct{ deviceID, root, mountPoint, filesystem, source, options string }

func mountUnescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+3 >= len(s) {
			return "", errors.New("invalid mount escape")
		}
		n, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil || n == 0 {
			return "", errors.New("invalid mount escape")
		}
		b.WriteByte(byte(n))
		i += 3
	}
	return b.String(), nil
}

func parseMounts(data []byte) ([]mountEntry, error) {
	var result []mountEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			return nil, errors.New("invalid mount information")
		}
		a, b := strings.Fields(left), strings.Fields(right)
		if len(a) < 6 || len(b) < 3 {
			return nil, errors.New("invalid mount information")
		}
		root, err := mountUnescape(a[3])
		if err != nil {
			return nil, err
		}
		point, err := mountUnescape(a[4])
		if err != nil {
			return nil, err
		}
		source, err := mountUnescape(b[1])
		if err != nil {
			return nil, err
		}
		result = append(result, mountEntry{a[2], root, point, b[0], source, a[5]})
	}
	return result, nil
}

func (d *Driver) validateVolume(v Volume) error {
	if runtime.GOOS != "linux" {
		return errors.New("block tracking requires Linux")
	}
	for _, p := range []string{v.Device, v.MountPoint, v.COWDir} {
		r, err := filepath.EvalSymlinks(p)
		if err != nil || r != p {
			return errors.New("approved volume paths changed")
		}
	}
	st, err := os.Stat(v.Device)
	if err != nil || st.Mode()&os.ModeDevice == 0 || st.Mode()&os.ModeCharDevice != 0 {
		return errors.New("source must be a block device")
	}
	if !strings.HasPrefix(v.Device, "/dev/") {
		return errors.New("source must be an approved /dev block device")
	}
	for _, prefix := range []string{"/dev/datto", "/dev/loop", "/dev/dm-", "/dev/md", "/dev/ram", "/dev/zram"} {
		if strings.HasPrefix(v.Device, prefix) {
			return errors.New("only ordinary data disks or partitions are supported")
		}
	}
	st, err = os.Stat(v.COWDir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("COW directory must be private (0700)")
	}
	data, err := readLimited(d.config.MountInfoPath, 4<<20)
	if err != nil {
		return errors.New("mount information unavailable")
	}
	mounts, err := parseMounts(data)
	if err != nil {
		return err
	}
	var selected *mountEntry
	for i, m := range mounts {
		if m.mountPoint == v.MountPoint {
			if selected != nil {
				return errors.New("ambiguous source mount")
			}
			selected = &mounts[i]
		}
	}
	if selected == nil || selected.root != "/" || (selected.filesystem != "ext4" && selected.filesystem != "xfs") {
		return errors.New("source must be a complete ext4 or XFS data mount")
	}
	if !strings.Contains(","+selected.options+",", ",rw,") {
		return errors.New("source data mount must be read-write")
	}
	source, err := filepath.EvalSymlinks(selected.source)
	if err != nil || source != v.Device {
		return errors.New("block device does not match approved mount")
	}
	for _, m := range mounts {
		if (m.mountPoint == "/" || pathWithin("/boot", m.mountPoint)) && m.deviceID == selected.deviceID {
			return errors.New("system block device cannot be tracked")
		}
		if m.mountPoint != v.MountPoint && pathWithin(v.MountPoint, m.mountPoint) && pathWithin(m.mountPoint, v.COWDir) {
			return errors.New("COW directory crosses a nested mount")
		}
	}
	return discardDisabled("/sys", selected.deviceID)
}

func discardDisabled(sysRoot, deviceID string) error {
	if !regexp.MustCompile(`^[0-9]+:[0-9]+$`).MatchString(deviceID) {
		return errors.New("invalid source device identity")
	}
	device, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "dev", "block", deviceID))
	if err != nil {
		return errors.New("source queue information unavailable")
	}
	if !pathWithin(sysRoot, device) || strings.Contains(filepath.ToSlash(device), "/devices/virtual/") {
		return errors.New("virtual or stacked block devices are unsupported")
	}
	if _, err := os.Stat(filepath.Join(device, "partition")); err == nil {
		device = filepath.Dir(device)
	} else if !os.IsNotExist(err) {
		return errors.New("cannot inspect source partition")
	}
	b, err := readLimited(filepath.Join(device, "queue", "discard_max_bytes"), 64)
	if err != nil || strings.TrimSpace(string(b)) != "0" {
		return errors.New("discard must be disabled on the source device queue (discard_max_bytes=0)")
	}
	slaves, err := os.ReadDir(filepath.Join(device, "slaves"))
	if err != nil || len(slaves) != 0 {
		return errors.New("cannot verify an ordinary unstacked source device")
	}
	return nil
}

func (d *Driver) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, d.config.DBDCtl, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("dbdctl command failed; tracking requires manual inspection")
	}
	return nil
}

func (d *Driver) volume(id string) (Volume, error) {
	for _, v := range d.config.Volumes {
		if v.ID == id {
			return v, nil
		}
	}
	return Volume{}, errors.New("volume is not approved")
}

func (d *Driver) begin(id string) (Volume, *journal, func(), error) {
	v, err := d.volume(id)
	if err != nil {
		return v, nil, nil, err
	}
	_, supported := d.platform()
	if !supported {
		return v, nil, nil, errors.New("unsupported platform: tracking requires Ubuntu 24.04 GA 6.8 generic amd64")
	}
	if err := d.validate(v); err != nil {
		return v, nil, nil, err
	}
	lock := d.config.StateFile + ".lock"
	if err := os.Mkdir(lock, 0700); err != nil {
		return v, nil, nil, errors.New("driver journal is locked; inspect previous process before manual recovery")
	}
	unlock := func() { _ = os.Remove(lock) }
	j, err := d.loadJournal()
	if err != nil {
		unlock()
		return v, nil, nil, err
	}
	return v, j, unlock, nil
}

func (d *Driver) loadJournal() (*journal, error) {
	j := &journal{Version: 1, Volumes: map[string]journalEntry{}}
	st, err := os.Lstat(d.config.StateFile)
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil || !st.Mode().IsRegular() || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
		return nil, errors.New("driver journal must be a private regular file")
	}
	b, err := readLimited(d.config.StateFile, 1<<20)
	if err != nil {
		return nil, errors.New("cannot read driver journal")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(j); err != nil || j.Version != 1 || j.Volumes == nil {
		return nil, errors.New("invalid driver journal; manual baseline required")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid driver journal trailing data")
	}
	return j, nil
}

func (d *Driver) saveJournal(j *journal) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(d.config.StateFile), ".cbt-journal-*")
	if err != nil {
		return errors.New("cannot prepare driver journal")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("cannot persist driver journal")
	}
	if err = os.Rename(name, d.config.StateFile); err != nil {
		return errors.New("cannot commit driver journal")
	}
	// Sync the parent entry as well: a successfully returned journal must survive
	// a crash before the following dbdctl command can change driver state.
	if runtime.GOOS != "windows" {
		parent, err := os.Open(filepath.Dir(d.config.StateFile))
		if err != nil {
			return errors.New("cannot open driver journal directory")
		}
		defer parent.Close()
		if err = parent.Sync(); err != nil {
			return errors.New("cannot sync driver journal directory")
		}
	}
	return nil
}

func (d *Driver) status(v Volume) (Generation, error) {
	info, err := d.readProc()
	if err != nil {
		return Generation{}, err
	}
	if info.Version != UpstreamVersion {
		return Generation{}, errors.New("dattobd version is not the validated 0.11.10")
	}
	for _, s := range info.Devices {
		if s.Minor != v.Minor {
			continue
		}
		if s.Error != nil || s.State == nil || (*s.State != 2 && *s.State != 3) || s.Sequence == 0 || !uuidPattern.MatchString(s.UUID) || s.Version > 1 {
			return Generation{}, errors.New("driver state is failed, dormant, unverified or unsupported")
		}
		device, err := filepath.EvalSymlinks(s.BlockDevice)
		if err != nil || device != v.Device {
			return Generation{}, errors.New("driver source does not match approved device")
		}
		// Active cow_file is relative to the source mount, despite its leading '/'.
		if !strings.HasPrefix(s.COWFile, "/") || strings.Contains(s.COWFile, "\\") {
			return Generation{}, errors.New("invalid driver COW path")
		}
		cow := filepath.Join(v.MountPoint, strings.TrimPrefix(s.COWFile, "/"))
		resolved, err := filepath.EvalSymlinks(cow)
		if err != nil || resolved != cow || !pathWithin(v.COWDir, cow) {
			return Generation{}, errors.New("driver COW file is outside approved directory")
		}
		st, err := os.Lstat(cow)
		if err != nil || !st.Mode().IsRegular() {
			return Generation{}, errors.New("driver COW file is unavailable")
		}
		mode := "incremental"
		if *s.State == 3 {
			mode = "snapshot"
		}
		size, err := d.size(v.Device)
		if err != nil {
			return Generation{}, err
		}
		snapshotDevice := fmt.Sprintf("/dev/datto%d", v.Minor)
		if mode == "snapshot" {
			snapshotSize, err := d.size(snapshotDevice)
			if err != nil || snapshotSize != size {
				return Generation{}, errors.New("snapshot size does not match approved device")
			}
		}
		return Generation{VolumeID: v.ID, UUID: s.UUID, COWFile: cow, SnapshotDevice: snapshotDevice, Sequence: s.Sequence, Minor: v.Minor, Mode: mode, Size: size}, nil
	}
	return Generation{}, errors.New("approved volume is not tracked; manual baseline required")
}

func (d *Driver) checked(v Volume, j *journal) (journalEntry, error) {
	entry, ok := j.Volumes[v.ID]
	if !ok {
		return entry, ErrNoGeneration
	}
	if entry.Pending {
		return entry, errors.New("tracking generation is missing or pending; manual baseline required")
	}
	boot, err := d.bootID()
	if err != nil {
		return entry, err
	}
	if entry.BootID != boot {
		return entry, errors.New("boot continuity lost; manual baseline required")
	}
	g, err := d.status(v)
	if err != nil {
		return entry, err
	}
	if g != entry.Current {
		return entry, errors.New("driver generation differs from durable journal; manual baseline required")
	}
	return entry, nil
}

func (d *Driver) newCOW(v Volume, name string) (string, error) {
	if !safeName.MatchString(name) {
		return "", errors.New("COW name must use 1–80 letters, numbers, underscores or hyphens")
	}
	path := filepath.Join(v.COWDir, name)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return "", errors.New("COW filename must never be reused")
	}
	return path, nil
}

func (d *Driver) Setup(ctx context.Context, id, cowName string) (Generation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Generation{}, err
	}
	v, j, unlock, err := d.begin(id)
	if err != nil {
		return Generation{}, err
	}
	defer unlock()
	if _, ok := j.Volumes[id]; ok {
		return Generation{}, errors.New("volume already has tracking history; manual baseline required")
	}
	info, err := d.readProc()
	if err != nil {
		return Generation{}, err
	}
	if info.Version != UpstreamVersion {
		return Generation{}, errors.New("dattobd version is not the validated 0.11.10")
	}
	for _, s := range info.Devices {
		device, _ := filepath.EvalSymlinks(s.BlockDevice)
		if s.Minor == v.Minor || device == v.Device {
			return Generation{}, errors.New("device or minor is already tracked outside this journal")
		}
	}
	cow, err := d.newCOW(v, cowName)
	if err != nil {
		return Generation{}, err
	}
	boot, err := d.bootID()
	if err != nil {
		return Generation{}, err
	}
	entry := journalEntry{Pending: true, BootID: boot}
	j.Volumes[id] = entry
	if err = d.saveJournal(j); err != nil {
		return Generation{}, err
	}
	if err = d.command(ctx, "setup-snapshot", v.Device, cow, strconv.FormatUint(uint64(v.Minor), 10)); err != nil {
		return Generation{}, err
	}
	g, err := d.status(v)
	if err != nil {
		return Generation{}, err
	}
	if g.COWFile != cow || g.Mode != "snapshot" || g.Sequence != 1 {
		return Generation{}, errors.New("unexpected setup result; journal remains pending")
	}
	entry.Current = g
	entry.Pending = false
	j.Volumes[id] = entry
	if err = d.saveJournal(j); err != nil {
		return Generation{}, err
	}
	return g, nil
}

func (d *Driver) Current(ctx context.Context, id string) (Transition, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Transition{}, err
	}
	v, j, unlock, err := d.begin(id)
	if err != nil {
		return Transition{}, err
	}
	defer unlock()
	entry, err := d.checked(v, j)
	if err != nil {
		return Transition{}, err
	}
	return entry.Transition, nil
}

func (d *Driver) Verify(ctx context.Context, id string, expected Generation) error {
	t, err := d.Current(ctx, id)
	if err != nil {
		return err
	}
	if t.Current != expected {
		return errors.New("unexpected tracking generation")
	}
	return nil
}

func (d *Driver) ToSnapshot(ctx context.Context, id, cowName string) (Transition, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Transition{}, err
	}
	v, j, unlock, err := d.begin(id)
	if err != nil {
		return Transition{}, err
	}
	defer unlock()
	entry, err := d.checked(v, j)
	if err != nil {
		return Transition{}, err
	}
	if entry.Current.Mode != "incremental" || entry.Current.Sequence == math.MaxUint64 {
		return Transition{}, errors.New("snapshot transition requires active incremental tracking")
	}
	cow, err := d.newCOW(v, cowName)
	if err != nil {
		return Transition{}, err
	}
	previous := entry.Current
	entry.Pending = true
	j.Volumes[id] = entry
	if err = d.saveJournal(j); err != nil {
		return Transition{}, err
	}
	if err = d.command(ctx, "transition-to-snapshot", cow, strconv.FormatUint(uint64(v.Minor), 10)); err != nil {
		return Transition{}, err
	}
	g, err := d.status(v)
	if err != nil {
		return Transition{}, err
	}
	if g.Mode != "snapshot" || g.UUID != previous.UUID || g.Sequence != previous.Sequence+1 || g.COWFile != cow {
		return Transition{}, errors.New("unexpected snapshot generation; journal remains pending")
	}
	entry.Current = g
	entry.PreviousCOW = previous.COWFile
	entry.PreviousSequence = previous.Sequence
	entry.Pending = false
	j.Volumes[id] = entry
	if err = d.saveJournal(j); err != nil {
		return Transition{}, err
	}
	return entry.Transition, nil
}

func (d *Driver) ToIncremental(ctx context.Context, id string, expected Generation) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	v, j, unlock, err := d.begin(id)
	if err != nil {
		return err
	}
	defer unlock()
	entry, err := d.checked(v, j)
	if err != nil {
		return err
	}
	if entry.Current != expected || expected.Mode != "snapshot" {
		return errors.New("incremental transition requires the expected active snapshot")
	}
	entry.Pending = true
	j.Volumes[id] = entry
	if err = d.saveJournal(j); err != nil {
		return err
	}
	if err = d.command(ctx, "transition-to-incremental", strconv.FormatUint(uint64(v.Minor), 10)); err != nil {
		return err
	}
	g, err := d.status(v)
	if err != nil {
		return err
	}
	expected.Mode = "incremental"
	if g != expected {
		return errors.New("unexpected incremental generation; journal remains pending")
	}
	entry.Current = g
	entry.Pending = false
	j.Volumes[id] = entry
	return d.saveJournal(j)
}

type Extent struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// ReadChanges decodes the documented native little-endian COW format on amd64.
// The caller supplies the OLD generation identity and source device byte size.
// Only a clean, closed, incremental index is accepted; active files are unsafe.
func ReadChanges(r io.ReaderAt, size int64, uuid string, prevSeq uint64) ([]Extent, error) {
	if size <= 0 || size%512 != 0 || prevSeq == 0 || !uuidPattern.MatchString(uuid) {
		return nil, errors.New("invalid COW source size or generation")
	}
	var header [4096]byte
	if _, err := r.ReadAt(header[:], 0); err != nil {
		return nil, errors.New("truncated COW header")
	}
	order := binary.LittleEndian
	flags := order.Uint32(header[4:8])
	if order.Uint32(header[0:4]) != 4776 || flags&3 != 3 || flags&^uint32(7) != 0 {
		return nil, errors.New("COW index must have valid magic and clean incremental flags")
	}
	version := order.Uint64(header[48:56])
	if version > 1 || order.Uint64(header[24:32]) != prevSeq || hex.EncodeToString(header[32:48]) != uuid {
		return nil, errors.New("COW format or generation mismatch")
	}
	blocks := (size + 4095) / 4096
	// At most 128 GiB of index for a 64 TiB device. Reads remain bounded, and a
	// fragmented adversarial bitmap cannot allocate unbounded extent memory.
	if blocks > (64<<40)/4096 {
		return nil, errors.New("source exceeds experimental 64 TiB limit")
	}
	extents := []Extent{}
	buffer := make([]byte, 64<<10)
	var changed uint64
	for first := int64(0); first < blocks; {
		count := int64(len(buffer) / 8)
		if count > blocks-first {
			count = blocks - first
		}
		chunk := buffer[:count*8]
		if _, err := r.ReadAt(chunk, 4096+first*8); err != nil {
			return nil, errors.New("truncated COW block index")
		}
		for i := int64(0); i < count; i++ {
			if order.Uint64(chunk[i*8:i*8+8]) == 0 {
				continue
			}
			changed++
			offset := (first + i) * 4096
			last := len(extents) - 1
			length := int64(4096)
			if length > size-offset {
				length = size - offset
			}
			if last >= 0 && extents[last].Offset+extents[last].Length == offset {
				extents[last].Length += length
			} else {
				if len(extents) >= 1<<20 {
					return nil, errors.New("COW extent count exceeds safety limit")
				}
				extents = append(extents, Extent{offset, length})
			}
		}
		first += count
	}
	if version == 1 && changed != order.Uint64(header[56:64]) {
		return nil, errors.New("COW changed-block count mismatch")
	}
	return extents, nil
}
