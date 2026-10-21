package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ichenwin/open-vault/internal/blockstore"
	"github.com/ichenwin/open-vault/internal/cbt"
)

func TestSnapshotReaderClosedBeforeFinalization(t *testing.T) {
	c, store, tracker, _ := coordinatorFixture(t)
	tracker.beforeFinalize = func() error {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			target, _ := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if target == tracker.source {
				return errors.New("snapshot reader still open at finalization")
			}
		}
		return nil
	}
	if _, err := backup(context.Background(), c, store, tracker, ""); err != nil {
		t.Fatal(err)
	}
}

func TestChainLimitRefusedBeforeRotation(t *testing.T) {
	c, store, tracker, data := coordinatorFixture(t)
	var parent string
	for seq := uint64(1); seq <= 128; seq++ {
		g, err := store.Commit(context.Background(), bytes.NewReader(data), blockstore.Request{Parent: parent, Size: int64(len(data)), SourceID: c.Volume.ID, TrackerUUID: series, Sequence: seq})
		if err != nil {
			t.Fatal(err)
		}
		parent = g.ID
	}
	tracker.current = cbt.Transition{Current: cbt.Generation{VolumeID: c.Volume.ID, UUID: series, Sequence: 128, Mode: "incremental", SnapshotDevice: tracker.source, Size: int64(len(data))}}
	if _, err := backup(context.Background(), c, store, tracker, ""); err == nil || tracker.rotates != 0 {
		t.Fatal("chain limit must preserve active incremental tracking", err, tracker.rotates)
	}
}
