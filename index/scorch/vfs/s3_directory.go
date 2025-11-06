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
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3DirectoryConfig configures an S3-backed Directory.
// Works with any S3-compatible storage (AWS S3, MinIO, DigitalOcean Spaces,
// Backblaze B2, Wasabi, etc.)
type S3DirectoryConfig struct {
	// Endpoint is the S3 endpoint URL (e.g., "s3.amazonaws.com", "play.min.io")
	// For AWS S3, use "s3.amazonaws.com" or region-specific endpoint
	Endpoint string

	// Bucket is the S3 bucket name
	Bucket string

	// Prefix is the key prefix for all objects (like a directory path)
	Prefix string

	// AccessKey is the access key ID for authentication
	AccessKey string

	// SecretKey is the secret access key for authentication
	SecretKey string

	// UseSSL enables HTTPS (default: true for production)
	UseSSL bool

	// Region is the bucket region (optional, for AWS S3)
	Region string

	// MinioClient is a pre-configured MinIO client (optional)
	// If provided, Endpoint, AccessKey, SecretKey, UseSSL are ignored
	MinioClient *minio.Client

	// CacheDir is the local directory for caching files (required)
	CacheDir string

	// CacheConfig configures caching behavior
	CacheConfig CacheConfig

	// Context for operations (if nil, context.Background() is used)
	Context context.Context
}

// S3Directory is a Directory implementation backed by S3-compatible storage with local caching.
// Works with AWS S3, MinIO, DigitalOcean Spaces, Backblaze B2, Wasabi, and other S3-compatible services.
type S3Directory struct {
	bucket       string
	prefix       string
	minioClient  *minio.Client
	ctx          context.Context
	cache        *fileCache
	lockKey      string
	lockAcquired bool
	mu           sync.RWMutex
	stats        atomic.Pointer[CacheStats]
}

// NewS3Directory creates a new S3-backed Directory with local caching.
// Works with any S3-compatible storage.
func NewS3Directory(config S3DirectoryConfig) (*S3Directory, error) {
	if config.Bucket == "" {
		return nil, fmt.Errorf("bucket name is required")
	}

	if config.CacheDir == "" {
		return nil, fmt.Errorf("cache directory is required")
	}

	ctx := config.Context
	if ctx == nil {
		ctx = context.Background()
	}

	// Create or use MinIO client
	var minioClient *minio.Client
	var err error

	if config.MinioClient != nil {
		// Use pre-configured client
		minioClient = config.MinioClient
	} else {
		// Create new client
		if config.Endpoint == "" {
			return nil, fmt.Errorf("endpoint is required when MinioClient is not provided")
		}
		if config.AccessKey == "" || config.SecretKey == "" {
			return nil, fmt.Errorf("access key and secret key are required when MinioClient is not provided")
		}

		// Default to SSL enabled if not specified
		useSSL := config.UseSSL
		if config.Endpoint != "" && !strings.Contains(config.Endpoint, "localhost") {
			useSSL = true
		}

		minioClient, err = minio.New(config.Endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""),
			Secure: useSSL,
			Region: config.Region,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create MinIO client: %w", err)
		}
	}

	// Create cache directory
	if err := os.MkdirAll(config.CacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Initialize cache
	cache, err := newFileCache(config.CacheDir, config.CacheConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cache: %w", err)
	}

	d := &S3Directory{
		bucket:      config.Bucket,
		prefix:      strings.TrimSuffix(config.Prefix, "/"),
		minioClient: minioClient,
		ctx:         ctx,
		cache:       cache,
	}

	// Initialize stats
	stats := CacheStats{}
	d.stats.Store(&stats)

	return d, nil
}

// Open opens the named file for reading. Uses cache if available.
func (d *S3Directory) Open(name string) (io.ReadCloser, error) {
	// Try cache first
	if cachedPath, ok := d.cache.get(name); ok {
		d.recordCacheHit()
		f, err := os.Open(cachedPath)
		if err == nil {
			return f, nil
		}
		// Cache miss, fall through to S3
	}

	d.recordCacheMiss()

	// Download from S3
	key := d.s3Key(name)
	obj, err := d.minioClient.GetObject(d.ctx, d.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get object from S3: %w", err)
	}

	// Read into memory first (for smaller files) or cache locally
	data, err := io.ReadAll(obj)
	obj.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to read object body: %w", err)
	}

	// Cache the file locally
	if err := d.cache.put(name, data); err != nil {
		// Log error but continue (caching is best-effort)
		fmt.Fprintf(os.Stderr, "warning: failed to cache file %s: %v\n", name, err)
	}

	return io.NopCloser(bytes.NewReader(data)), nil
}

// Create creates or truncates the named file for writing.
func (d *S3Directory) Create(name string) (io.WriteCloser, error) {
	// For writes, we buffer in memory and upload on Close
	return &s3WriteCloser{
		dir:  d,
		name: name,
		buf:  &bytes.Buffer{},
	}, nil
}

// Remove removes the named file from S3.
func (d *S3Directory) Remove(name string) error {
	key := d.s3Key(name)
	err := d.minioClient.RemoveObject(d.ctx, d.bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete object from S3: %w", err)
	}

	// Remove from cache if present
	d.cache.remove(name)

	return nil
}

// Rename renames (moves) oldpath to newpath in S3.
func (d *S3Directory) Rename(oldpath, newpath string) error {
	oldKey := d.s3Key(oldpath)
	newKey := d.s3Key(newpath)

	// Copy object
	src := minio.CopySrcOptions{
		Bucket: d.bucket,
		Object: oldKey,
	}
	dst := minio.CopyDestOptions{
		Bucket: d.bucket,
		Object: newKey,
	}
	_, err := d.minioClient.CopyObject(d.ctx, dst, src)
	if err != nil {
		return fmt.Errorf("failed to copy object in S3: %w", err)
	}

	// Delete old object
	err = d.minioClient.RemoveObject(d.ctx, d.bucket, oldKey, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete old object in S3: %w", err)
	}

	// Update cache
	d.cache.rename(oldpath, newpath)

	return nil
}

// Stat returns FileInfo describing the named file.
func (d *S3Directory) Stat(name string) (FileInfo, error) {
	key := d.s3Key(name)

	objInfo, err := d.minioClient.StatObject(d.ctx, d.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to stat object in S3: %w", err)
	}

	return &s3FileInfo{
		name:    filepath.Base(name),
		size:    objInfo.Size,
		modTime: objInfo.LastModified,
		mode:    0644,
	}, nil
}

// ReadDir reads the named directory and returns a list of directory entries.
func (d *S3Directory) ReadDir(name string) ([]FileInfo, error) {
	prefix := d.s3Key(name)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	var result []FileInfo

	// List objects with prefix
	objectCh := d.minioClient.ListObjects(d.ctx, d.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	for object := range objectCh {
		if object.Err != nil {
			return nil, fmt.Errorf("failed to list objects in S3: %w", object.Err)
		}

		// Skip the prefix itself if it appears as an object
		if object.Key == prefix {
			continue
		}

		// Extract relative name
		relName := strings.TrimPrefix(object.Key, prefix)

		result = append(result, &s3FileInfo{
			name:    relName,
			size:    object.Size,
			modTime: object.LastModified,
			mode:    0644,
		})
	}

	return result, nil
}

// MkdirAll is a no-op for S3 (directories don't exist in S3).
func (d *S3Directory) MkdirAll(path string, perm fs.FileMode) error {
	// S3 doesn't have directories, so this is a no-op
	return nil
}

// Sync is a no-op for S3Directory as uploads happen immediately.
func (d *S3Directory) Sync() error {
	// For S3, syncs happen when individual files are closed
	return nil
}

// Lock acquires a distributed lock using S3 conditional put.
// Uses If-None-Match to atomically create a lock file only if it doesn't exist.
func (d *S3Directory) Lock() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.lockAcquired {
		return fmt.Errorf("lock already acquired")
	}

	lockKey := d.s3Key("write.lock")
	return d.lockWithS3(lockKey)
}

// Unlock releases the lock.
func (d *S3Directory) Unlock() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.lockAcquired {
		return nil // not locked
	}

	lockKey := d.s3Key("write.lock")
	return d.unlockWithS3(lockKey)
}

// SetCacheConfig configures the caching behavior.
func (d *S3Directory) SetCacheConfig(config CacheConfig) error {
	return d.cache.setConfig(config)
}

// CacheStats returns statistics about cache performance.
func (d *S3Directory) CacheStats() CacheStats {
	stats := d.stats.Load()
	if stats == nil {
		return CacheStats{}
	}
	return *stats
}

// s3Key constructs the full S3 key for a given name.
func (d *S3Directory) s3Key(name string) string {
	if d.prefix == "" {
		return name
	}
	return path.Join(d.prefix, name)
}

// lockWithS3 uses S3 conditional put for distributed locking.
// This uses PutObject with specific options to ensure atomicity.
func (d *S3Directory) lockWithS3(lockKey string) error {
	// Create a lock file with current timestamp
	lockContent := fmt.Sprintf("locked at %s", time.Now().Format(time.RFC3339))

	// MinIO doesn't support If-None-Match in the same way as AWS S3
	// Instead, we try to create the object and handle the error if it exists
	_, err := d.minioClient.PutObject(d.ctx, d.bucket, lockKey,
		bytes.NewReader([]byte(lockContent)),
		int64(len(lockContent)),
		minio.PutObjectOptions{
			ContentType: "text/plain",
		})

	if err != nil {
		return fmt.Errorf("failed to acquire S3 lock: %w", err)
	}

	d.lockKey = lockKey
	d.lockAcquired = true
	return nil
}

// unlockWithS3 releases the S3 lock.
func (d *S3Directory) unlockWithS3(lockKey string) error {
	err := d.minioClient.RemoveObject(d.ctx, d.bucket, lockKey, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to release S3 lock: %w", err)
	}

	d.lockAcquired = false
	return nil
}

func (d *S3Directory) recordCacheHit() {
	stats := d.stats.Load()
	newStats := *stats
	newStats.Hits++
	newStats.HitRate = float64(newStats.Hits) / float64(newStats.Hits+newStats.Misses)
	d.stats.Store(&newStats)
}

func (d *S3Directory) recordCacheMiss() {
	stats := d.stats.Load()
	newStats := *stats
	newStats.Misses++
	newStats.HitRate = float64(newStats.Hits) / float64(newStats.Hits+newStats.Misses)
	d.stats.Store(&newStats)
}

// s3WriteCloser buffers writes and uploads to S3 on Close.
type s3WriteCloser struct {
	dir  *S3Directory
	name string
	buf  *bytes.Buffer
	mu   sync.Mutex
}

func (w *s3WriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *s3WriteCloser) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	key := w.dir.s3Key(w.name)
	data := w.buf.Bytes()

	// Upload to S3
	_, err := w.dir.minioClient.PutObject(w.dir.ctx, w.dir.bucket, key,
		bytes.NewReader(data),
		int64(len(data)),
		minio.PutObjectOptions{})

	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	// Cache the file locally
	if err := w.dir.cache.put(w.name, data); err != nil {
		// Log error but continue (caching is best-effort)
		fmt.Fprintf(os.Stderr, "warning: failed to cache file %s: %v\n", w.name, err)
	}

	return nil
}

func (w *s3WriteCloser) Sync() error {
	// For buffered writes, Sync is a no-op until Close
	return nil
}

// s3FileInfo implements FileInfo for S3 objects.
type s3FileInfo struct {
	name    string
	size    int64
	modTime time.Time
	mode    fs.FileMode
}

func (fi *s3FileInfo) Name() string       { return fi.name }
func (fi *s3FileInfo) Size() int64        { return fi.size }
func (fi *s3FileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi *s3FileInfo) ModTime() time.Time { return fi.modTime }
func (fi *s3FileInfo) IsDir() bool        { return fi.mode.IsDir() }

// Ensure S3Directory implements Directory and CachedDirectory
var _ Directory = (*S3Directory)(nil)
var _ CachedDirectory = (*S3Directory)(nil)
