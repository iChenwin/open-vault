package blockstore

import "errors"

// MaxVolumeSize bounds the experimental JSON-manifest repository format.
const MaxVolumeSize int64 = 256 << 30

// ValidateCapacity rejects oversized or excessively fragmented generations
// without reading the snapshot. Each record's JSON is at most 206 bytes for
// four nonnegative int64 fields and a SHA256; 224 includes its separator.
// Bounded identity strings and 8192 reserved bytes cover generation metadata.
func ValidateCapacity(req Request) error {
	if req.Size <= 0 || req.Size > MaxVolumeSize {
		return errors.New("experimental block repository requires a volume of at most 256 GiB")
	}
	if len(req.SourceID) > 256 || len(req.TrackerUUID) > 256 {
		return errors.New("source and tracker identities must be at most 256 bytes")
	}
	ranges, err := extents(req)
	if err != nil {
		return err
	}
	var records int64
	for _, extent := range ranges {
		records += (extent.Length + chunkSize - 1) / chunkSize
		if records > (manifestLimit-8192)/224 {
			return errors.New("generation is too fragmented for the experimental manifest; snapshot must be retained for operator review")
		}
	}
	return nil
}

// CheckAppend checks volume and chain capacity before a tracker rotates. The
// caller holds its repository process lock. Commit repeats these checks.
func (s *Store) CheckAppend(parent string, size int64) error {
	if size <= 0 || size > MaxVolumeSize {
		return errors.New("experimental block repository requires a volume of at most 256 GiB")
	}
	if parent == "" {
		return nil
	}
	layers, err := s.chain(parent)
	if err != nil {
		return err
	}
	defer closeLayers(layers)
	if len(layers) >= maxChain {
		return errors.New("generation chain limit reached; operator must establish a new repository and tracking baseline")
	}
	return nil
}
