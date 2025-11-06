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

```go
import (
    "context"
    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/s3"
    "github.com/blevesearch/bleve/v2/index/scorch/vfs"
)

// Load AWS configuration
cfg, err := config.LoadDefaultConfig(context.Background())
if err != nil {
    // handle error
}

// Create S3 client
s3Client := s3.NewFromConfig(cfg)

// Configure S3 directory
s3Config := vfs.S3DirectoryConfig{
    Bucket:   "my-index-bucket",
    Prefix:   "indexes/my-index",
    Region:   "us-east-1",
    S3Client: s3Client,
    CacheDir: "/tmp/bleve-cache",
    CacheConfig: vfs.CacheConfig{
        MaxCacheSizeBytes: 1024 * 1024 * 1024, // 1GB
        MaxCacheEntries:   1000,
        EvictionPolicy:    "lru",
    },
    LazyLoad: true,
}

// Create S3-backed directory
dir, err := vfs.NewS3Directory(s3Config)
if err != nil {
    // handle error
}

// Use with Scorch...
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

- **Object storage backend**: Stores segments in S3
- **Local caching**: LRU cache for frequently accessed files
- **Lazy loading**: Only downloads files when accessed
- **Distributed locking**:
  - DynamoDB-based locking (recommended for production)
  - S3-based locking (fallback, less reliable)
- **Cache statistics**: Monitor hit rates and evictions
- **Configurable**: Adjustable cache size, eviction policy, etc.

### Caching Strategy

The S3 implementation uses a two-level caching strategy:

1. **In-memory buffers**: Small files are buffered in memory
2. **Local disk cache**: Larger files are cached on local disk with LRU eviction

This provides:
- Fast access to frequently used segments
- Reduced S3 API calls and costs
- Automatic cleanup of stale cache entries

## Distributed Locking

For S3 directories, distributed locking is crucial to prevent multiple writers from corrupting the index. Two options are supported:

### DynamoDB Locking (Recommended)

```go
import "github.com/aws/aws-sdk-go-v2/service/dynamodb"

dynamoClient := dynamodb.NewFromConfig(cfg)

s3Config := vfs.S3DirectoryConfig{
    // ... other config ...
    DynamoDBClient: dynamoClient,
    LockTableName:  "bleve-index-locks",
}
```

Create the DynamoDB table:

```bash
aws dynamodb create-table \
    --table-name bleve-index-locks \
    --attribute-definitions \
        AttributeName=LockKey,AttributeType=S \
    --key-schema \
        AttributeName=LockKey,KeyType=HASH \
    --billing-mode PAY_PER_REQUEST \
    --time-to-live-specification \
        Enabled=true,AttributeName=TTL
```

### S3 Locking (Fallback)

If DynamoDB is not available, S3 locking uses conditional PUT operations. This is less reliable but works in simple scenarios:

```go
s3Config := vfs.S3DirectoryConfig{
    // ... other config ...
    DynamoDBClient: nil, // No DynamoDB client
}
```

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
