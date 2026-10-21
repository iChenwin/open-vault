package cbt

import (
	"context"
	"errors"
	"io"
	"os"
)

type cancellableIndex struct {
	context.Context
	io.ReaderAt
}

func (r cancellableIndex) ReadAt(buffer []byte, offset int64) (int, error) {
	if err := r.Context.Err(); err != nil {
		return 0, err
	}
	return r.ReaderAt.ReadAt(buffer, offset)
}

// Changes owns the provider's on-disk index protocol. Coordinators receive
// verified byte extents, rather than interpreting a kernel-specific COW file.
func (d *Driver) Changes(ctx context.Context, id string, expected Generation) ([]Extent, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	v, journal, unlock, err := d.begin(id)
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	entry, err := d.checked(v, journal)
	if err != nil {
		return nil, 0, err
	}
	if entry.Current != expected || expected.Mode != "snapshot" || entry.PreviousSequence == 0 || entry.PreviousCOW == "" {
		return nil, 0, errors.New("closed change index does not match the expected snapshot")
	}
	index, err := os.Open(entry.PreviousCOW)
	if err != nil {
		return nil, 0, err
	}
	defer index.Close()
	ranges, err := ReadChanges(cancellableIndex{ctx, index}, expected.Size, expected.UUID, entry.PreviousSequence)
	if err != nil {
		return nil, 0, err
	}
	return ranges, 4096 + ((expected.Size+4095)/4096)*8, nil
}
