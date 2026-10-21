package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ichenwin/open-vault/internal/cbt"
)

func TestSourceFilesystemCannotContainRepositoryThroughAlias(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if e := os.Mkdir(source, 0700); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(root, "alias")
	if e := os.Symlink(source, alias); e != nil {
		t.Fatal(e)
	}
	c := config{Volume: cbt.Volume{MountPoint: source}, Repository: filepath.Join(alias, "vault"), KeyFile: filepath.Join(root, "key"), StateDir: filepath.Join(root, "state")}
	if e := validateDiskSeparation(c, ""); e == nil {
		t.Fatal("accepted same source filesystem via alias")
	}
}
