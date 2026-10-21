package cbt

import (
	"context"
	"encoding/binary"
	"os"
	"reflect"
	"testing"
)

func TestDriverChangesUsesClosedPreviousGeneration(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	first, err := f.d.Setup(ctx, "data", "first")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.d.ToIncremental(ctx, "data", first); err != nil {
		t.Fatal(err)
	}
	next, err := f.d.ToSnapshot(ctx, "data", "second")
	if err != nil {
		t.Fatal(err)
	}
	index := cowBytes(t, int(first.Size/4096), 1, 2, 7)
	binary.LittleEndian.PutUint64(index[24:32], first.Sequence)
	if err = os.WriteFile(next.PreviousCOW, index, 0600); err != nil {
		t.Fatal(err)
	}
	ranges, read, err := f.d.Changes(ctx, "data", next.Current)
	if err != nil || read != int64(len(index)) || !reflect.DeepEqual(ranges, []Extent{{4096, 8192}, {7 * 4096, 4096}}) {
		t.Fatalf("wrong verified extents: %v %d %v", ranges, read, err)
	}
	if _, _, err = f.d.Changes(ctx, "data", first); err == nil {
		t.Fatal("accepted stale snapshot")
	}
	index[32]++
	if err = os.WriteFile(next.PreviousCOW, index, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.d.Changes(ctx, "data", next.Current); err == nil {
		t.Fatal("accepted another tracking UUID")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err = f.d.Changes(cancelled, "data", next.Current); err == nil {
		t.Fatal("ignored cancellation")
	}
}
