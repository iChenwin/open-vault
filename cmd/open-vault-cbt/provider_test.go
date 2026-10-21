package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ichenwin/open-vault/internal/blksnap"
	"github.com/ichenwin/open-vault/internal/cbt"
)

func TestProviderSelectionIsExplicitAndRejectsMixedOptions(t *testing.T) {
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(cache, "open-vault-provider-test")
	if err = os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(base, "case-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	c := config{StateDir: filepath.Join(root, "state"), Provider: "blksnap", Blksnap: filepath.Join(root, "bin", "blksnap"), Volume: cbt.Volume{ID: "data", Device: filepath.Join(root, "device"), MountPoint: filepath.Join(root, "source"), COWDir: filepath.Join(root, "cow")}}
	for _, path := range []string{filepath.Join(c.StateDir, "blksnap"), filepath.Dir(c.Blksnap), c.Volume.MountPoint, c.Volume.COWDir} {
		if err = os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{c.Blksnap, c.Volume.Device} {
		if err = os.WriteFile(path, []byte("fixture"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	driver, err := newTracker(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := driver.(*blksnap.Driver); !ok {
		t.Fatal("did not select external COW provider")
	}
	for _, mutate := range []func(*config){
		func(c *config) { c.Provider = "unknown" },
		func(c *config) { c.Provider = "" },
		func(c *config) { c.DBDCtl = filepath.Join(root, "dbdctl") },
		func(c *config) { c.Blksnap = "relative-path" },
	} {
		bad := c
		mutate(&bad)
		if _, err := newTracker(bad); err == nil {
			t.Fatal("accepted ambiguous or untrusted provider configuration")
		}
	}
}
