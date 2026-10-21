package cbt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCOWCannotBeReplacedThroughWritableSourceParent(t *testing.T) {
	mount := t.TempDir()
	parent := filepath.Join(mount, "control")
	cow := filepath.Join(parent, "cow")
	if e := os.MkdirAll(cow, 0700); e != nil {
		t.Fatal(e)
	}
	if e := protectCOWHierarchy(mount, cow); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(parent, 0777); e != nil {
		t.Fatal(e)
	}
	if e := protectCOWHierarchy(mount, cow); e == nil {
		t.Fatal("private COW leaf accepted beneath writable parent")
	}
}
