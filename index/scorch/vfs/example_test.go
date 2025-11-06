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

package vfs

import (
	"fmt"
	"io"
	"testing"
)

// TestHybridDirectory demonstrates how the hybrid approach works
func TestHybridDirectory(t *testing.T) {
	// Create two directories: one for metadata, one for segments
	metadataDir, err := NewFSDirectory(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create metadata directory: %v", err)
	}

	segmentDir, err := NewFSDirectory(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create segment directory: %v", err)
	}

	// Create hybrid directory
	hybrid := NewHybridDirectory(segmentDir, metadataDir)

	// Lock the hybrid directory
	if err := hybrid.Lock(); err != nil {
		t.Fatalf("Failed to lock hybrid directory: %v", err)
	}
	defer hybrid.Unlock()

	// Write a metadata file (goes to metadata dir)
	metaFile := "root.bolt"
	w, err := hybrid.Create(metaFile)
	if err != nil {
		t.Fatalf("Failed to create metadata file: %v", err)
	}
	w.Write([]byte("metadata"))
	w.Close()

	// Write a segment file (goes to segment dir)
	segFile := "00000001.zap"
	w, err = hybrid.Create(segFile)
	if err != nil {
		t.Fatalf("Failed to create segment file: %v", err)
	}
	w.Write([]byte("segment data"))
	w.Close()

	// Verify files are routed correctly
	// Metadata should be in metadata directory
	if _, err := metadataDir.Stat(metaFile); err != nil {
		t.Errorf("Metadata file not found in metadata directory")
	}

	// Segment should be in segment directory
	if _, err := segmentDir.Stat(segFile); err != nil {
		t.Errorf("Segment file not found in segment directory")
	}

	// ReadDir should show both files
	files, err := hybrid.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to read directory: %v", err)
	}

	// Should have at least 2 files (metadata + segment) plus possible lock files
	if len(files) < 2 {
		t.Errorf("Expected at least 2 files, got %d", len(files))
	}

	// Verify we can read both files through hybrid directory
	r, err := hybrid.Open(metaFile)
	if err != nil {
		t.Fatalf("Failed to open metadata file: %v", err)
	}
	metaData, _ := io.ReadAll(r)
	r.Close()

	if string(metaData) != "metadata" {
		t.Errorf("Wrong metadata content: got %q, want %q", metaData, "metadata")
	}

	r, err = hybrid.Open(segFile)
	if err != nil {
		t.Fatalf("Failed to open segment file: %v", err)
	}
	segData, _ := io.ReadAll(r)
	r.Close()

	if string(segData) != "segment data" {
		t.Errorf("Wrong segment content: got %q, want %q", segData, "segment data")
	}
}

// TestDirectoryAdapter demonstrates segment opening with cache
func TestDirectoryAdapter(t *testing.T) {
	tmpDir := t.TempDir()
	dir, err := NewFSDirectory(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create directory: %v", err)
	}

	adapter := NewDirectoryAdapter(dir, tmpDir)

	// Create a mock segment file
	segmentName := "00000001.zap"
	w, err := dir.Create(segmentName)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	w.Write([]byte("segment content"))
	w.Close()

	// Open segment through adapter
	path, closer, err := adapter.OpenSegment(segmentName)
	if err != nil {
		t.Fatalf("Failed to open segment: %v", err)
	}
	defer closer.Close()

	// For filesystem directories, path should be the real absolute path
	if path == "" {
		t.Error("Expected non-empty path")
	}

	t.Logf("Segment opened at: %s", path)
}

// Example showing the full workflow
func ExampleHybridDirectory() {
	// Simulate S3 directory with filesystem for demo
	s3SimDir, _ := NewFSDirectory("/tmp/segments")
	metadataDir, _ := NewFSDirectory("/tmp/metadata")

	hybrid := NewHybridDirectory(s3SimDir, metadataDir)
	hybrid.Lock()
	defer hybrid.Unlock()

	// Write metadata (stays local)
	meta, _ := hybrid.Create("root.bolt")
	meta.Write([]byte("index metadata"))
	meta.Close()

	// Write segments (could be S3)
	seg, _ := hybrid.Create("00000001.zap")
	seg.Write([]byte("segment 1 data"))
	seg.Close()

	seg, _ = hybrid.Create("00000002.zap")
	seg.Write([]byte("segment 2 data"))
	seg.Close()

	// List all files
	files, _ := hybrid.ReadDir(".")
	// We have at least 3 files: root.bolt, 00000001.zap, 00000002.zap
	// (plus possibly lock files)
	fmt.Printf("Has segment files: %v\n", len(files) >= 3)

	// Output: Has segment files: true
}
