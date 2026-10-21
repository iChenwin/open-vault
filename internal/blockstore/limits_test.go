package blockstore

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestManifestCapacityBoundsBeforeSourceRead(t *testing.T) {
	r := Request{Size: MaxVolumeSize, SourceID: "data", TrackerUUID: "tracker", Sequence: 1}
	if err := ValidateCapacity(r); err != nil {
		t.Fatal("256 GiB baseline must fit conservative bound", err)
	}
	b, err := json.Marshal(record{Extent: Extent{math.MaxInt64, math.MaxInt64}, PayloadOffset: math.MaxInt64, CipherSize: math.MaxInt64, SHA256: strings.Repeat("f", 64)})
	if err != nil || len(b)+1 > 224 {
		t.Fatal("manifest record bound no longer valid", len(b), err)
	}
	s := newTestStore(t)
	r.Size++
	reader := &countedReader{}
	if _, err := s.Commit(context.Background(), reader, r); err == nil || reader.count != 0 || len(reader.reads) != 0 {
		t.Fatal("oversized volume was not rejected before reads", err)
	}
	r.Size = MaxVolumeSize
	r.Parent = strings.Repeat("a", 32)
	for i := int64(0); i < 310000; i++ {
		r.Changed = append(r.Changed, Extent{Offset: i * 8192, Length: 4096})
	}
	if err := ValidateCapacity(r); err == nil || !strings.Contains(err.Error(), "fragmented") {
		t.Fatal("fragmented incremental manifest accepted", err)
	}
	if _, err := s.Commit(context.Background(), reader, r); err == nil || len(reader.reads) != 0 {
		t.Fatal("fragmented generation read snapshot", err)
	}
}
