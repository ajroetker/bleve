# Integrating VFS with Scorch

This document shows **exactly** how to integrate VFS's storage abstraction into Scorch.

## The Hybrid Approach (Recommended)

Since Bolt DB requires a filesystem path (for mmap), we use a **hybrid approach**:
- **Segment files (.zap)**: Stored in pluggable Directory (can be S3)
- **Metadata (root.bolt)**: Stored on local filesystem

This gives us the best of both worlds:
- ✅ Large segment files can use S3 with caching
- ✅ Small metadata stays fast on local disk
- ✅ Bolt DB works without modification
- ✅ Minimal changes to Scorch

## Step 1: Add Directory Field to Scorch

**File: `index/scorch/scorch.go`**

```go
type Scorch struct {
    nextSegmentID uint64
    stats         Stats
    iStats        internalStats

    readOnly      bool
    version       uint8
    config        map[string]interface{}
    analysisQueue *index.AnalysisQueue
    path          string

    // ADD THIS: Directory for segment storage
    segmentDir    vfs.Directory  // ← NEW FIELD

    unsafeBatch bool
    // ... rest of fields
}
```

## Step 2: Initialize Directory in NewScorch

**File: `index/scorch/scorch.go`**

```go
func NewScorch(storeName string,
    config map[string]interface{},
    analysisQueue *index.AnalysisQueue,
) (index.Index, error) {
    rv := &Scorch{
        version:              Version,
        config:               config,
        analysisQueue:        analysisQueue,
        nextSnapshotEpoch:    1,
        closeCh:              make(chan struct{}),
        ineligibleForRemoval: map[string]bool{},
        forceMergeRequestCh:  make(chan *mergerCtrl, 1),
        segPlugin:            defaultSegmentPlugin,
        copyScheduled:        map[string]int{},
    }

    // NEW: Check if custom directory is provided
    if dirConfig, ok := config["segmentDirectory"].(vfs.Directory); ok {
        rv.segmentDir = dirConfig
    }

    // ... rest of initialization
}
```

## Step 3: Update openBolt to Use Hybrid Directory

**File: `index/scorch/scorch.go`**

```go
func (s *Scorch) openBolt() error {
    var ok bool
    s.path, ok = s.config["path"].(string)
    if !ok {
        return fmt.Errorf("must specify path")
    }

    // NEW: If no segment directory provided, use filesystem
    if s.segmentDir == nil {
        var err error
        s.segmentDir, err = vfs.NewFSDirectory(s.path)
        if err != nil {
            return fmt.Errorf("failed to create filesystem directory: %w", err)
        }
    }

    if s.path == "" {
        s.unsafeBatch = true
    }

    rootBoltOpt := *bolt.DefaultOptions
    if s.readOnly {
        rootBoltOpt.ReadOnly = true
        rootBoltOpt.OpenFile = func(path string, flag int, mode os.FileMode) (*os.File, error) {
            if _, err := os.Stat(path); os.IsNotExist(err) {
                return os.OpenFile(path, flag, mode)
            }
            return os.OpenFile(path, os.O_RDONLY, mode)
        }
    } else {
        // Create path for metadata (bolt DB stays on filesystem)
        if s.path != "" {
            err := os.MkdirAll(s.path, 0o700)
            if err != nil {
                return err
            }
        }
    }

    // ... rest of bolt initialization (unchanged)
}
```

## Step 4: Update Segment Persistence

**File: `index/scorch/persister.go`**

The good news: persister.go **already** has partial support for `index.Directory`!

We just need to change line 771 from:
```go
filenames, newSegmentPaths, err := prepareBoltSnapshot(snapshot, tx, s.path, s.segPlugin, exclude, nil)
```

To:
```go
filenames, newSegmentPaths, err := prepareBoltSnapshot(snapshot, tx, s.path, s.segPlugin, exclude, s.segmentDir)
```

And update `persistToDirectory` to use our Directory interface:

```go
func persistSegmentToDirectory(seg segment.UnpersistedSegment, dir vfs.Directory, path string) error {
    if dir == nil {
        return seg.Persist(path)
    }

    sg, ok := seg.(io.WriterTo)
    if !ok {
        return fmt.Errorf("no io.WriterTo segment implementation found")
    }

    w, err := dir.Create(filepath.Base(path))
    if err != nil {
        return err
    }
    defer w.Close()

    _, err = sg.WriteTo(w)
    return err
}
```

## Step 5: Update Segment Loading

**File: `index/scorch/persister.go` - loadSegment function**

```go
func (s *Scorch) loadSegment(segmentBucket *bolt.Bucket) (*SegmentSnapshot, error) {
    pathBytes := segmentBucket.Get(util.BoltPathKey)
    if pathBytes == nil {
        return nil, fmt.Errorf("segment path missing")
    }

    segmentName := string(pathBytes)

    // NEW: Use segment directory to open segment
    segmentPath := segmentName
    if s.path != "" {
        segmentPath = s.path + string(os.PathSeparator) + segmentName
    }

    // Open segment using pluggable directory
    seg, err := s.openSegmentFile(segmentPath)
    if err != nil {
        return nil, fmt.Errorf("error opening segment: %v", err)
    }

    rv := &SegmentSnapshot{
        segment:    seg,
        cachedDocs: &cachedDocs{cache: nil},
        cachedMeta: &cachedMeta{meta: nil},
    }

    // ... rest of function unchanged
}

// NEW: Helper function to open segment files via Directory
func (s *Scorch) openSegmentFile(path string) (segment.Segment, error) {
    // For S3 or other non-filesystem directories, we need to download
    // the file to local cache and open it from there
    if cached, ok := s.segmentDir.(vfs.CachedDirectory); ok {
        return s.openSegmentCached(path, cached)
    }

    // For filesystem directories, just open directly
    return s.segPlugin.Open(path)
}

func (s *Scorch) openSegmentCached(name string, dir vfs.CachedDirectory) (segment.Segment, error) {
    // Download segment to local temp location
    r, err := dir.Open(filepath.Base(name))
    if err != nil {
        return nil, err
    }
    defer r.Close()

    // Create temp file for segment
    tmpFile, err := os.CreateTemp("", "segment-*.zap")
    if err != nil {
        return nil, err
    }

    // Copy data
    if _, err := io.Copy(tmpFile, r); err != nil {
        tmpFile.Close()
        os.Remove(tmpFile.Name())
        return nil, err
    }
    tmpFile.Close()

    // Open segment from temp location
    seg, err := s.segPlugin.Open(tmpFile.Name())
    if err != nil {
        os.Remove(tmpFile.Name())
        return nil, err
    }

    return &cachedSegment{
        Segment:  seg,
        tempPath: tmpFile.Name(),
    }, nil
}

// Wrapper to clean up temp files when segment is closed
type cachedSegment struct {
    segment.Segment
    tempPath string
}

func (c *cachedSegment) Close() error {
    err := c.Segment.Close()
    _ = os.Remove(c.tempPath) // Best effort cleanup
    return err
}
```

## Step 6: Update File Removal

**File: `index/scorch/persister.go` - removeOldZapFiles**

```go
func (s *Scorch) removeOldZapFiles() error {
    liveFileNames, err := s.loadZapFileNames()
    if err != nil {
        return err
    }

    // Use segmentDir to list and remove files
    files, err := s.segmentDir.ReadDir(".")
    if err != nil {
        return err
    }

    s.rootLock.RLock()
    defer s.rootLock.RUnlock()

    for _, f := range files {
        fname := f.Name()
        if filepath.Ext(fname) == ".zap" {
            if _, exists := liveFileNames[fname]; !exists &&
               !s.ineligibleForRemoval[fname] &&
               (s.copyScheduled[fname] <= 0) {

                // Use Directory interface to remove
                err := s.segmentDir.Remove(fname)
                if err != nil {
                    log.Printf("got err removing file: %s, err: %v", fname, err)
                }
            }
        }
    }

    return nil
}
```

## Step 7: Update diskFileStats

**File: `index/scorch/scorch.go`**

```go
func (s *Scorch) diskFileStats(rootSegmentPaths map[string]struct{}) (uint64, uint64, uint64) {
    var numFilesOnDisk, numBytesUsedDisk, numBytesOnDiskByRoot uint64

    // Use segmentDir to read directory
    files, err := s.segmentDir.ReadDir(".")
    if err == nil {
        for _, f := range files {
            if !f.IsDir() {
                numBytesUsedDisk += uint64(f.Size())
                numFilesOnDisk++
                if rootSegmentPaths != nil {
                    fname := f.Name()
                    if _, fileAtRoot := rootSegmentPaths[fname]; fileAtRoot {
                        numBytesOnDiskByRoot += uint64(f.Size())
                    }
                }
            }
        }
    }

    // if no root files path given, then consider all disk files.
    if rootSegmentPaths == nil {
        return numFilesOnDisk, numBytesUsedDisk, numBytesUsedDisk
    }

    return numFilesOnDisk, numBytesUsedDisk, numBytesOnDiskByRoot
}
```

## Complete Usage Example

Here's how you'd create a Scorch index with S3 storage:

```go
package main

import (
    "context"
    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/s3"
    "github.com/blevesearch/bleve/v2"
    "github.com/blevesearch/bleve/v2/index/scorch/vfs"
    "github.com/blevesearch/bleve/v2/mapping"
)

func main() {
    // Load AWS config
    cfg, _ := config.LoadDefaultConfig(context.Background())
    s3Client := s3.NewFromConfig(cfg)

    // Create S3 directory for segments
    segmentDir, _ := vfs.NewS3Directory(vfs.S3DirectoryConfig{
        Bucket:   "my-index-bucket",
        Prefix:   "indexes/my-index",
        S3Client: s3Client,
        CacheDir: "/tmp/bleve-cache",
        CacheConfig: vfs.CacheConfig{
            MaxCacheSizeBytes: 2 * 1024 * 1024 * 1024, // 2GB cache
            MaxCacheEntries:   1000,
            EvictionPolicy:    "lru",
        },
        LazyLoad: true,
    })

    // Create index mapping
    indexMapping := mapping.NewIndexMapping()

    // Create index with custom segment directory
    index, err := bleve.NewUsing("/tmp/bleve-metadata", indexMapping,
        "scorch", "scorch", map[string]interface{}{
            "path":            "/tmp/bleve-metadata",  // Metadata stays local
            "segmentDirectory": segmentDir,             // Segments in S3
        })

    if err != nil {
        panic(err)
    }
    defer index.Close()

    // Index some data
    index.Index("doc1", map[string]interface{}{
        "title": "Hello VFS",
        "body":  "This segment is stored in S3!",
    })

    // Search works normally
    query := bleve.NewMatchQuery("vfs")
    search := bleve.NewSearchRequest(query)
    results, _ := index.Search(search)

    // Check cache stats
    if cached, ok := segmentDir.(vfs.CachedDirectory); ok {
        stats := cached.CacheStats()
        fmt.Printf("Cache hit rate: %.2f%%\n", stats.HitRate * 100)
        fmt.Printf("Cache size: %d bytes\n", stats.CacheSizeBytes)
    }
}
```

## What Changes Are Required?

### Minimal Changes to Scorch Core:

1. **Add field**: `segmentDir vfs.Directory` to Scorch struct
2. **Initialize field**: In `NewScorch()` and `openBolt()`
3. **Replace calls**:
   - `os.ReadDir()` → `s.segmentDir.ReadDir()`
   - `os.Remove()` → `s.segmentDir.Remove()`
   - Pass `s.segmentDir` instead of `nil` to persistence functions
4. **Add helpers**: `openSegmentFile()` and `openSegmentCached()`

### Benefits:

✅ **Backward compatible**: If no `segmentDirectory` config, uses filesystem
✅ **S3 support**: Segments can be stored in S3 with local caching
✅ **Bolt DB unchanged**: Metadata stays fast on local disk
✅ **Minimal changes**: ~200 lines of code changes in Scorch
✅ **Extensible**: Easy to add new storage backends

## Testing the Integration

```bash
# Run Scorch tests with filesystem directory (should all pass)
go test ./index/scorch -v

# Run with S3 directory (requires AWS credentials)
export AWS_PROFILE=my-profile
go test ./index/scorch -v -tags=s3integration
```

## Performance Expectations

### Filesystem Directory
- Same as current Scorch (zero overhead)
- All operations <1ms

### S3 Directory
- **First access**: 10-50ms (download from S3)
- **Cached access**: <1ms (local cache hit)
- **Expected hit rate**: 90%+ with proper cache sizing
- **Write latency**: ~50ms (upload to S3)

## Conclusion

**Yes, we CAN use this storage interface in Scorch!**

The integration is straightforward because:
1. We use a hybrid approach (segments pluggable, metadata local)
2. Scorch already has some Directory abstraction in persister.go
3. Changes are localized to ~5 functions
4. Backward compatibility is maintained

The result: **Serverless Scorch with S3 storage** 🚀
