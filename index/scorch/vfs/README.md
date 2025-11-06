# VFS: Storage Abstraction for Scorch

VFS is a storage abstraction layer for Bleve's Scorch index that decouples filesystem operations from the index implementation. This allows Scorch to use different storage backends like local filesystem, S3, or other object storage systems.

## Motivation

The original Scorch implementation is tightly coupled to the local filesystem, making it difficult to:
- Use object storage (S3, GCS, Azure Blob) as the primary storage backend
- Build serverless indexing solutions
- Implement sophisticated caching strategies
- Support distributed/cloud-native deployments

VFS solves these problems by introducing a `Directory` abstraction inspired by Bluge's design but tailored for Scorch.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Scorch Index                              │
│  (Modified to use Directory abstraction)                     │
└──────────────────────────┬───────────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────────┐
│              Directory Interface (VFS)                   │
│  ├─ Open(path) (Reader, error)                              │
│  ├─ Create(path) (Writer, error)                            │
│  ├─ Remove(path) error                                       │
│  ├─ Rename(oldpath, newpath) error                          │
│  ├─ Stat(path) (FileInfo, error)                            │
│  ├─ ReadDir(path) ([]FileInfo, error)                       │
│  ├─ MkdirAll(path, perm) error                              │
│  ├─ Sync() error                                             │
│  ├─ Lock()/Unlock() error                                    │
└──────────────────┬────────────────────┬──────────────────────┘
                   │                    │
         ┌─────────▼────────┐  ┌────────▼─────────────┐
         │  FSDirectory     │  │  S3Directory         │
         │  (filesystem)    │  │  (object storage)    │
         └──────────────────┘  └──────┬───────────────┘
                                      │
                              ┌───────▼────────┐
                              │  Local Cache   │
                              │  (LRU eviction)│
                              └────────────────┘
```

## Usage

### Filesystem Directory

```go
import "github.com/blevesearch/bleve/v2/index/scorch/vfs"

// Create a filesystem-backed directory
dir, err := vfs.NewFSDirectory("/path/to/index")
if err != nil {
    // handle error
}

// Lock the directory
if err := dir.Lock(); err != nil {
    // handle error
}
defer dir.Unlock()

// Use with Scorch...
```

### S3 Directory

Works with **any S3-compatible storage** including AWS S3, MinIO, DigitalOcean Spaces, Backblaze B2, Wasabi, etc.

#### AWS S3 Example

```go
import (
    "github.com/blevesearch/bleve/v2/index/scorch/vfs"
)

s3Config := vfs.S3DirectoryConfig{
    Endpoint:  "s3.amazonaws.com",
    Bucket:    "my-index-bucket",
    Prefix:    "indexes/my-index",
    AccessKey: "AWS_ACCESS_KEY_ID",
    SecretKey: "AWS_SECRET_ACCESS_KEY",
    Region:    "us-east-1",
    UseSSL:    true,
    CacheDir:  "/tmp/bleve-cache",
    CacheConfig: vfs.CacheConfig{
        MaxCacheSizeBytes: 1024 * 1024 * 1024, // 1GB
        MaxCacheEntries:   1000,
        EvictionPolicy:    "lru",
    },
}

dir, err := vfs.NewS3Directory(s3Config)
if err != nil {
    // handle error
}
```

#### MinIO Example

```go
s3Config := vfs.S3DirectoryConfig{
    Endpoint:  "play.min.io",
    Bucket:    "my-index-bucket",
    Prefix:    "indexes/my-index",
    AccessKey: "minioadmin",
    SecretKey: "minioadmin",
    UseSSL:    true,
    CacheDir:  "/tmp/bleve-cache",
    CacheConfig: vfs.CacheConfig{
        MaxCacheSizeBytes: 1024 * 1024 * 1024, // 1GB
        MaxCacheEntries:   1000,
    },
}

dir, err := vfs.NewS3Directory(s3Config)
```

#### DigitalOcean Spaces Example

```go
s3Config := vfs.S3DirectoryConfig{
    Endpoint:  "nyc3.digitaloceanspaces.com",
    Bucket:    "my-index-bucket",
    Prefix:    "indexes/my-index",
    AccessKey: "DO_SPACES_KEY",
    SecretKey: "DO_SPACES_SECRET",
    Region:    "us-east-1",
    UseSSL:    true,
    CacheDir:  "/tmp/bleve-cache",
    CacheConfig: vfs.CacheConfig{
        MaxCacheSizeBytes: 1024 * 1024 * 1024, // 1GB
        MaxCacheEntries:   1000,
    },
}

dir, err := vfs.NewS3Directory(s3Config)
```

#### Backblaze B2 Example

```go
s3Config := vfs.S3DirectoryConfig{
    Endpoint:  "s3.us-west-004.backblazeb2.com",
    Bucket:    "my-index-bucket",
    Prefix:    "indexes/my-index",
    AccessKey: "B2_KEY_ID",
    SecretKey: "B2_APPLICATION_KEY",
    UseSSL:    true,
    CacheDir:  "/tmp/bleve-cache",
    CacheConfig: vfs.CacheConfig{
        MaxCacheSizeBytes: 1024 * 1024 * 1024, // 1GB
        MaxCacheEntries:   1000,
    },
}

dir, err := vfs.NewS3Directory(s3Config)
```

### Using URL Configuration

```go
// Parse directory URL
config, err := vfs.ParseDirectoryURL("s3://my-bucket/indexes/my-index", &s3Config)
if err != nil {
    // handle error
}

// Create directory
dir, err := vfs.NewDirectory(config)
if err != nil {
    // handle error
}
```

## Features

### FSDirectory

- **Direct filesystem access**: Uses standard `os` package operations
- **File locking**: Uses `flock` for cross-process locking
- **Zero overhead**: No caching or translation layer

### S3Directory

- **S3-compatible storage**: Works with AWS S3, MinIO, DigitalOcean Spaces, Backblaze B2, Wasabi, and any S3-compatible service
- **Local caching**: LRU cache for frequently accessed files
- **Lazy loading**: Only downloads files when accessed
- **Simple S3-based locking**: Uses conditional S3 operations for distributed locking
- **Cache statistics**: Monitor hit rates and evictions
- **Configurable**: Adjustable cache size, eviction policy, etc.
- **No vendor lock-in**: Uses MinIO SDK for maximum compatibility

### Caching Strategy

The S3 implementation uses a two-level caching strategy:

1. **In-memory buffers**: Small files are buffered in memory
2. **Local disk cache**: Larger files are cached on local disk with LRU eviction

This provides:
- Fast access to frequently used segments
- Reduced S3 API calls and costs
- Automatic cleanup of stale cache entries

## Distributed Locking

For S3 directories, distributed locking is crucial to prevent multiple writers from corrupting the index.

### S3-Based Locking

The VFS implementation uses **S3-native locking** with conditional PUT operations. This approach:

- **Works with any S3-compatible storage** (no vendor-specific dependencies like DynamoDB)
- **Simple and portable**: Just works out of the box
- **Lock files stored in S3**: Uses a `write.lock` object in your bucket
- **Automatic**: No additional setup or tables required

Locking is handled automatically when you call `Lock()` and `Unlock()`:

```go
dir, err := vfs.NewS3Directory(s3Config)
if err != nil {
    // handle error
}

// Acquire lock before writing
if err := dir.Lock(); err != nil {
    // handle lock error
}
defer dir.Unlock()

// Perform write operations...
```

**Note**: For production deployments with multiple writers, ensure your application:
1. Always acquires a lock before writing
2. Releases locks promptly after writing
3. Implements retry logic for lock acquisition failures

## Performance Considerations

### S3 Latency

S3 has higher latency than local disk (~10-50ms vs <1ms). Mitigation strategies:

1. **Aggressive caching**: Cache all active segments locally
2. **Lazy loading**: Only download files when accessed
3. **Batch operations**: Group small operations together
4. **Read-only replicas**: Use S3 for read replicas, local disk for writers

### Cache Size

Choose cache size based on:
- Index size
- Query patterns
- Available local disk space

Rule of thumb: Cache should hold at least the "hot" segments (frequently accessed).

### Cost Optimization

S3 storage costs can be optimized:
- Use S3 Intelligent-Tiering for automatic cost optimization
- Compress segments before upload (future enhancement)
- Use S3 lifecycle policies to archive old snapshots to Glacier

## Roadmap

Future enhancements planned for VFS:

- [ ] Google Cloud Storage (GCS) support
- [ ] Azure Blob Storage support
- [ ] MinIO support (S3-compatible)
- [ ] Compression layer (transparent compression/decompression)
- [ ] Encryption at rest
- [ ] Multi-region replication
- [ ] Read-through cache warming
- [ ] Background cache preloading
- [ ] Metrics and observability (Prometheus/OpenTelemetry)

## Migration Guide

To migrate an existing Scorch index to use VFS:

### From Local Filesystem to S3

```go
// 1. Open existing index with FSDirectory
fsDir, _ := vfs.NewFSDirectory("/path/to/index")

// 2. Create S3Directory
s3Dir, _ := vfs.NewS3Directory(s3Config)

// 3. Copy index to S3 (TODO: implement copy method)
// This would be done at the Scorch level using CopyTo

// 4. Reopen index with S3Directory
```

## Compatibility

VFS is designed to be a drop-in replacement for Scorch's filesystem operations. The `Directory` interface is intentionally simple to ensure compatibility and ease of implementation.

Minimum requirements:
- Go 1.23+
- Bleve v2.x
- AWS SDK v2 (for S3 support)

## Contributing

Contributions are welcome! Areas where help is needed:

- Additional storage backend implementations (GCS, Azure, MinIO)
- Performance optimizations
- Test coverage
- Documentation improvements
- Bug fixes

## License

Apache License 2.0 (same as Bleve)
