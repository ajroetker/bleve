//  Copyright (c) 2025 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package firebug

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ScorchConfig extends the standard Scorch configuration to support
// pluggable storage via the Directory interface.
type ScorchConfig struct {
	// SegmentDirectory is used for storing segment files (.zap files)
	// If nil, falls back to filesystem at Path
	SegmentDirectory Directory

	// MetadataPath is where bolt DB metadata is stored
	// This must be a filesystem path (bolt requires mmap)
	MetadataPath string

	// Path is the legacy filesystem path (for backward compatibility)
	// Used if SegmentDirectory is nil
	Path string
}

// HybridDirectory wraps a Directory for segments while keeping
// bolt DB metadata on the local filesystem. This is the recommended
// approach for S3-backed indexes.
type HybridDirectory struct {
	segments Directory  // For segment files (can be S3)
	metadata Directory  // For bolt DB (must be filesystem)
}

// NewHybridDirectory creates a directory that stores segments in one
// location (e.g., S3) and metadata in another (local filesystem).
func NewHybridDirectory(segments, metadata Directory) *HybridDirectory {
	return &HybridDirectory{
		segments: segments,
		metadata: metadata,
	}
}

// Open opens a file - routes to segments or metadata based on file extension
func (h *HybridDirectory) Open(name string) (io.ReadCloser, error) {
	if h.isMetadata(name) {
		return h.metadata.Open(name)
	}
	return h.segments.Open(name)
}

// Create creates a file - routes based on file extension
func (h *HybridDirectory) Create(name string) (io.WriteCloser, error) {
	if h.isMetadata(name) {
		return h.metadata.Create(name)
	}
	return h.segments.Create(name)
}

// Remove removes a file
func (h *HybridDirectory) Remove(name string) error {
	if h.isMetadata(name) {
		return h.metadata.Remove(name)
	}
	return h.segments.Remove(name)
}

// Rename renames a file
func (h *HybridDirectory) Rename(oldpath, newpath string) error {
	if h.isMetadata(oldpath) || h.isMetadata(newpath) {
		return h.metadata.Rename(oldpath, newpath)
	}
	return h.segments.Rename(oldpath, newpath)
}

// Stat returns file info
func (h *HybridDirectory) Stat(name string) (FileInfo, error) {
	if h.isMetadata(name) {
		return h.metadata.Stat(name)
	}
	return h.segments.Stat(name)
}

// ReadDir reads a directory - combines both locations
func (h *HybridDirectory) ReadDir(name string) ([]FileInfo, error) {
	// For root directory, combine both
	if name == "." || name == "" {
		var all []FileInfo

		// Get metadata files
		metaFiles, err := h.metadata.ReadDir(name)
		if err == nil {
			all = append(all, metaFiles...)
		}

		// Get segment files
		segFiles, err := h.segments.ReadDir(name)
		if err == nil {
			all = append(all, segFiles...)
		}

		return all, nil
	}

	// For subdirectories, route appropriately
	return h.segments.ReadDir(name)
}

// MkdirAll creates directories in both locations
func (h *HybridDirectory) MkdirAll(path string, perm os.FileMode) error {
	// Create in both locations (ignore errors if already exists)
	_ = h.metadata.MkdirAll(path, perm)
	return h.segments.MkdirAll(path, perm)
}

// Sync syncs both locations
func (h *HybridDirectory) Sync() error {
	if err := h.metadata.Sync(); err != nil {
		return err
	}
	return h.segments.Sync()
}

// Lock locks both locations
func (h *HybridDirectory) Lock() error {
	// Lock metadata first (local filesystem lock)
	if err := h.metadata.Lock(); err != nil {
		return err
	}

	// Then lock segments (might be distributed lock for S3)
	if err := h.segments.Lock(); err != nil {
		h.metadata.Unlock() // Rollback
		return err
	}

	return nil
}

// Unlock unlocks both locations
func (h *HybridDirectory) Unlock() error {
	var err1, err2 error
	err1 = h.segments.Unlock()
	err2 = h.metadata.Unlock()

	if err1 != nil {
		return err1
	}
	return err2
}

// isMetadata returns true if the file is metadata (bolt DB)
func (h *HybridDirectory) isMetadata(name string) bool {
	ext := filepath.Ext(name)
	return ext == ".bolt" || name == "write.lock"
}

// DirectoryAdapter adapts a Firebug Directory to work with Scorch's
// existing segment plugin interface.
type DirectoryAdapter struct {
	dir      Directory
	basePath string // Virtual base path for this directory
}

// NewDirectoryAdapter creates an adapter that makes a Directory
// work with Scorch's segment plugins that expect filesystem paths.
func NewDirectoryAdapter(dir Directory, basePath string) *DirectoryAdapter {
	return &DirectoryAdapter{
		dir:      dir,
		basePath: basePath,
	}
}

// GetAbsolutePath returns a "path" for a segment - this is used
// by segment plugins. For filesystem directories, this is a real path.
// For S3 directories, this is a virtual path that the adapter intercepts.
func (d *DirectoryAdapter) GetAbsolutePath(name string) string {
	if d.basePath == "" {
		return name
	}
	return filepath.Join(d.basePath, name)
}

// OpenSegment opens a segment file and returns a path that segment
// plugins can use. For S3 directories, this downloads the file to
// a local cache and returns the cache path.
func (d *DirectoryAdapter) OpenSegment(name string) (string, io.Closer, error) {
	// Check if this is a cached directory (S3Directory)
	if cached, ok := d.dir.(CachedDirectory); ok {
		// For cached directories, ensure file is in local cache
		r, err := d.dir.Open(name)
		if err != nil {
			return "", nil, err
		}
		defer r.Close()

		// Read file into memory
		data, err := io.ReadAll(r)
		if err != nil {
			return "", nil, err
		}

		// Get cache stats to find local path
		// This is a workaround - in a real implementation, we'd expose
		// a GetCachePath method on CachedDirectory
		_ = cached.CacheStats()

		// For now, create a temp file
		tmpFile, err := os.CreateTemp("", "segment-*.zap")
		if err != nil {
			return "", nil, err
		}

		if _, err := tmpFile.Write(data); err != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			return "", nil, err
		}

		tmpFile.Close()

		return tmpFile.Name(), &segmentCloser{path: tmpFile.Name()}, nil
	}

	// For filesystem directories, just return the real path
	if fsDir, ok := d.dir.(*FSDirectory); ok {
		absPath := fsDir.fullPath(name)
		return absPath, io.NopCloser(nil), nil
	}

	return "", nil, fmt.Errorf("unsupported directory type for segment opening")
}

type segmentCloser struct {
	path string
}

func (c *segmentCloser) Close() error {
	// Clean up temp file
	return os.Remove(c.path)
}

// Ensure HybridDirectory implements Directory
var _ Directory = (*HybridDirectory)(nil)
