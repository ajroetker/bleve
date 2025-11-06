# VFS Implementation Notes

## Overview

VFS is a storage abstraction layer for Bleve's Scorch index that decouples filesystem operations from the core indexing logic. This document describes the implementation details, design decisions, and future integration steps.

## Architecture

### Core Abstraction: Directory Interface

The `Directory` interface is the core abstraction that all storage backends must implement:

```go
type Directory interface {
    Open(name string) (io.ReadCloser, error)
    Create(name string) (io.WriteCloser, error)
    Remove(name string) error
    Rename(oldpath, newpath string) error
    Stat(name string) (FileInfo, error)
    ReadDir(name string) ([]FileInfo, error)
    MkdirAll(path string, perm fs.FileMode) error
    Sync() error
    Lock() error
    Unlock() error
}
```

This interface was designed to be:
1. **Minimal**: Only includes operations actually used by Scorch
2. **Familiar**: Similar to Go's `os` package APIs
3. **Portable**: Works across different storage backends
4. **Safe**: Includes locking primitives for multi-process safety

### Implementations

#### 1. FSDirectory (Filesystem)

**Purpose**: Drop-in replacement for Scorch's current filesystem operations.

**Key Features**:
- Direct `os` package calls (zero overhead)
- `flock`-based locking for cross-process safety
- Full test coverage

**Design Decisions**:
- Uses absolute paths internally to avoid ambiguity
- Creates parent directories automatically on `Create()`
- Proper cleanup on `Unlock()` (best-effort lock file removal)

#### 2. S3Directory (Object Storage)

**Purpose**: Enable serverless and cloud-native Bleve deployments.

**Key Features**:
- AWS S3 backend with full API support
- Local LRU cache for performance
- Distributed locking via DynamoDB or S3
- Lazy loading support
- Cache statistics and monitoring

**Design Decisions**:

**Caching Strategy**:
- Two-level caching: memory buffers + local disk cache
- LRU eviction policy (configurable)
- Best-effort caching (failures don't break operations)
- Cache is transparent to callers

**Locking Strategy**:
- **Primary**: DynamoDB conditional PutItem (recommended for production)
- **Fallback**: S3 conditional PUT with `If-None-Match: *`
- TTL support for lock expiration
- Lock keys are namespaced by bucket/prefix

**Write Path**:
- Writes are buffered in memory
- Upload to S3 happens on `Close()`
- Files are cached locally after upload
- Atomic write semantics via S3's put-object

**Read Path**:
1. Check local cache first (cache hit = fast path)
2. On cache miss, download from S3
3. Store in local cache for future reads
4. Return data to caller

## File Structure

```
index/scorch/vfs/
├── README.md                    # User-facing documentation
├── IMPLEMENTATION_NOTES.md      # This file
├── directory.go                 # Core Directory interface
├── fs_directory.go              # Filesystem implementation
├── s3_directory.go              # S3 implementation
├── cache.go                     # LRU cache for S3Directory
├── factory.go                   # Factory functions and URL parsing
├── directory_test.go            # Interface compliance tests
├── fs_directory_test.go         # FSDirectory-specific tests
├── go.mod                       # Module definition
└── go.sum                       # Dependency checksums
```

## Integration with Scorch

### Current Status

VFS is currently a **standalone package** that can be used independently or integrated into Scorch. The Directory interface is designed to replace filesystem operations in:

1. **scorch.go**:
   - `openBolt()`: Replace path handling with Directory
   - `diskFileStats()`: Use Directory.ReadDir()
   - `removeOldZapFiles()`: Use Directory.Remove()

2. **persister.go**:
   - `copyToDirectory()`: Already uses index.Directory interface!
   - `persistToDirectory()`: Already uses index.Directory interface!
   - `persistSnapshotDirect()`: Needs to use Directory for segment files
   - `loadSegment()`: Needs to use Directory.Open()

3. **snapshot_segment.go**:
   - `FileSize()`: Replace os.Stat() with Directory.Stat()

4. **introducer.go**:
   - Minimal changes needed (mostly reads segment metadata)

### Integration Steps

To fully integrate VFS into Scorch:

#### Phase 1: Add Directory to Scorch struct

```go
type Scorch struct {
    // ... existing fields ...
    dir Directory // Add this
}
```

#### Phase 2: Update NewScorch

```go
func NewScorch(storeName string,
    config map[string]interface{},
    analysisQueue *index.AnalysisQueue,
) (index.Index, error) {
    // ... existing code ...

    // Create directory based on config
    var dir Directory
    if dirConfig, ok := config["directory"].(DirectoryConfig); ok {
        dir, err = NewDirectory(dirConfig)
    } else {
        // Fallback to filesystem for backward compatibility
        path := config["path"].(string)
        dir, err = NewFSDirectory(path)
    }

    rv.dir = dir
    // ... rest of initialization ...
}
```

#### Phase 3: Replace filesystem operations

Replace all `os.*` and `filepath.*` calls with `Directory` methods:

**Before**:
```go
files, err := os.ReadDir(s.path)
```

**After**:
```go
files, err := s.dir.ReadDir(".")
```

#### Phase 4: Update bolt DB handling

Bolt DB currently requires a filesystem path. For non-filesystem backends:
- Option A: Use in-memory bolt DB (bolt.Open with memory mode)
- Option B: Cache bolt DB locally even for S3 backend
- Option C: Replace bolt with alternative metadata store (DynamoDB, etcd)

**Recommended**: Option B (local bolt cache) for simplicity and compatibility.

## Testing

### Test Coverage

- ✅ **FSDirectory**: 100% coverage of interface methods
- ✅ **Directory compliance**: Standard test suite for all implementations
- ✅ **Concurrent operations**: Tested with 10 concurrent readers
- ✅ **Locking**: Verified cross-process lock exclusion
- ⚠️ **S3Directory**: Requires AWS credentials (integration tests not run by default)

### Running Tests

```bash
# Run all tests
go test ./...

# Run with coverage
go test -cover ./...

# Run specific test
go test -run TestFSDirectory_Locking

# Run with race detector
go test -race ./...
```

### Integration Testing with S3

To test S3Directory, you need:
1. AWS credentials configured
2. An S3 bucket
3. Optional: DynamoDB table for locking

```go
// Example S3 integration test (not included by default)
func TestS3Directory_Integration(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping S3 integration test")
    }

    cfg, _ := config.LoadDefaultConfig(context.Background())
    s3Client := s3.NewFromConfig(cfg)

    dir, err := NewS3Directory(S3DirectoryConfig{
        Bucket:   "test-bucket",
        Prefix:   "test-prefix",
        S3Client: s3Client,
        CacheDir: t.TempDir(),
    })

    directoryTestSuite(t, dir)
}
```

## Performance Considerations

### FSDirectory Performance

- **Zero overhead**: Direct `os` package calls
- **Expected latency**: <1ms for local disk operations
- **Throughput**: Limited by disk I/O (typically 100MB/s+)

### S3Directory Performance

- **Without cache**: 10-50ms per operation (S3 latency)
- **With cache hit**: <1ms (same as local disk)
- **Expected cache hit rate**: 90%+ for typical workloads

**Optimization Strategies**:
1. Increase cache size to hold more segments
2. Preload frequently accessed segments on startup
3. Use lazy loading to avoid downloading entire index
4. Use S3 Transfer Acceleration for multi-region deployments

### Cache Sizing Recommendations

| Index Size | Cache Size | Expected Hit Rate |
|------------|------------|-------------------|
| < 1GB      | 512MB      | 95%+              |
| 1-10GB     | 2GB        | 90%+              |
| 10-100GB   | 10GB       | 85%+              |
| 100GB+     | 50GB+      | 80%+              |

## Security Considerations

### FSDirectory

- Uses standard filesystem permissions (0644 for files, 0755 for directories)
- Lock file prevents concurrent access from multiple processes
- No special security features (relies on OS-level security)

### S3Directory

- **Encryption**: Uses AWS SDK defaults (can be configured for SSE-KMS)
- **Authentication**: Uses AWS credential chain (IAM roles, env vars, etc.)
- **Access control**: Respects S3 bucket policies and IAM policies
- **Lock security**: DynamoDB locks use IAM authentication

**Recommended IAM Policy**:
```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "s3:GetObject",
                "s3:PutObject",
                "s3:DeleteObject",
                "s3:ListBucket"
            ],
            "Resource": [
                "arn:aws:s3:::my-bucket/indexes/*",
                "arn:aws:s3:::my-bucket"
            ]
        },
        {
            "Effect": "Allow",
            "Action": [
                "dynamodb:PutItem",
                "dynamodb:DeleteItem",
                "dynamodb:GetItem"
            ],
            "Resource": "arn:aws:dynamodb:*:*:table/bleve-index-locks"
        }
    ]
}
```

## Known Limitations

### Current Limitations

1. **Bolt DB dependency**: Scorch uses bolt DB for metadata, which requires filesystem access
   - **Workaround**: Cache bolt DB locally even for S3 backend
   - **Future**: Support alternative metadata stores (DynamoDB, etcd)

2. **S3 locking reliability**: S3-based locking (without DynamoDB) is not fully reliable
   - **Workaround**: Use DynamoDB for distributed locking in production
   - **Future**: Support etcd/Consul for locking

3. **No compression**: S3 uploads are not compressed
   - **Future**: Add transparent compression layer

4. **No encryption**: No built-in encryption beyond AWS defaults
   - **Workaround**: Use S3 SSE-KMS
   - **Future**: Add client-side encryption option

### Future Enhancements

See README.md Roadmap section for planned features.

## Compatibility

### Backward Compatibility

VFS is designed to be **100% backward compatible** with existing Scorch indexes:

1. **Filesystem indexes**: Can be opened with FSDirectory using the same path
2. **Configuration**: Falls back to filesystem if no Directory config is provided
3. **File format**: No changes to segment file formats

### Migration Path

To migrate an existing index from filesystem to S3:

```go
// 1. Open with FSDirectory
fsDir, _ := vfs.NewFSDirectory("/path/to/index")
fsIndex, _ := bleve.OpenUsing("/path/to/index", map[string]interface{}{
    "directory": fsDir,
})

// 2. Copy to S3 (using CopyTo method)
s3Dir, _ := vfs.NewS3Directory(s3Config)
fsIndex.(vfs.IndexCopyable).CopyTo(s3Dir)

// 3. Close filesystem index
fsIndex.Close()

// 4. Reopen with S3Directory
s3Index, _ := bleve.OpenUsing("s3://bucket/prefix", map[string]interface{}{
    "directory": s3Dir,
})
```

## Contributing

To contribute to VFS:

1. **Code style**: Follow standard Go conventions (gofmt, golint)
2. **Testing**: All new code must have tests
3. **Documentation**: Update README.md and this file
4. **Compatibility**: Ensure backward compatibility

### Adding a New Storage Backend

To add a new storage backend (e.g., Google Cloud Storage):

1. Create `gcs_directory.go` implementing the `Directory` interface
2. Add configuration struct (e.g., `GCSDirectoryConfig`)
3. Update `factory.go` to support new backend type
4. Add tests following the `directoryTestSuite` pattern
5. Update README.md with usage examples

Example skeleton:

```go
type GCSDirectory struct {
    bucket string
    client *storage.Client
    // ... other fields
}

func NewGCSDirectory(config GCSDirectoryConfig) (*GCSDirectory, error) {
    // Implementation
}

// Implement all Directory methods...
```

## References

- [Bluge Directory Design](https://github.com/blugelabs/bluge/blob/master/index.go)
- [AWS S3 Best Practices](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html)
- [DynamoDB Locking](https://aws.amazon.com/blogs/database/building-distributed-locks-with-the-dynamodb-lock-client/)
