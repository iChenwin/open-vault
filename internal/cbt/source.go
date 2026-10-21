package cbt

// HostPlatform reports the explicitly scoped kernel-driver target. Matching it
// alone is not a successful backup acceptance test.
func HostPlatform() (string, bool) { return hostPlatform() }

// DataVolumeSize queries block-device size without reading its data payload.
func DataVolumeSize(path string) (int64, error) { return deviceSize(path) }

// ValidateExternalVolume reuses the ordinary data-volume, mount, ownership and
// discard checks, but requires a private COW directory on another filesystem.
// It is read-only and does not install or load any driver.
func ValidateExternalVolume(v Volume) error {
	d := &Driver{config: DriverConfig{MountInfoPath: "/proc/self/mountinfo"}}
	return d.validateVolumeWithLocation(v, true)
}
