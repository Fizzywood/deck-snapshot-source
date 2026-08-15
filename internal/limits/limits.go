// Package limits centralizes snapshot resource bounds.
package limits

import (
	"errors"
	"fmt"
	"math"
)

// Limits bounds discovery, archive creation and archive validation.
type Limits struct {
	MaxFiles            int
	MaxFileSize         int64
	MaxTotalSize        int64
	MaxManifestSize     int64
	MaxPathLength       int
	MaxCompressionRatio int64
}

func Default() Limits {
	return Limits{
		MaxFiles:            20_000,
		MaxFileSize:         128 << 20,
		MaxTotalSize:        2 << 30,
		MaxManifestSize:     8 << 20,
		MaxPathLength:       1024,
		MaxCompressionRatio: 200,
	}
}

func (l Limits) Validate() error {
	if l.MaxFiles < 1 {
		return errors.New("max files must be positive")
	}
	if l.MaxFileSize < 1 || l.MaxTotalSize < l.MaxFileSize {
		return errors.New("size limits are invalid")
	}
	if l.MaxManifestSize < 1 || l.MaxManifestSize > l.MaxFileSize {
		return errors.New("manifest size limit is invalid")
	}
	if l.MaxTotalSize > math.MaxInt64-l.MaxManifestSize {
		return errors.New("combined size limits overflow")
	}
	if l.MaxPathLength < 64 {
		return errors.New("path length limit is too small")
	}
	if l.MaxCompressionRatio < 1 {
		return errors.New("compression ratio limit must be positive")
	}
	return nil
}

// Counter applies file-count and size limits incrementally.
type Counter struct {
	Limits Limits
	Files  int
	Bytes  int64
}

func (c *Counter) Add(size int64) error {
	if size < 0 {
		return errors.New("file size is negative")
	}
	if size > c.Limits.MaxFileSize {
		return fmt.Errorf("file exceeds %d-byte limit", c.Limits.MaxFileSize)
	}
	if c.Files >= c.Limits.MaxFiles {
		return fmt.Errorf("file count exceeds %d-entry limit", c.Limits.MaxFiles)
	}
	if size > c.Limits.MaxTotalSize-c.Bytes {
		return fmt.Errorf("total size exceeds %d-byte limit", c.Limits.MaxTotalSize)
	}
	c.Files++
	c.Bytes += size
	return nil
}
