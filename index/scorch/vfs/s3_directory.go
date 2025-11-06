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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// S3DirectoryConfig configures an S3-backed Directory.
type S3DirectoryConfig struct {
	// Bucket is the S3 bucket name
	Bucket string

	// Prefix is the key prefix for all objects (like a directory path)
	Prefix string

	// Region is the AWS region
	Region string

	// S3Client is the AWS S3 client (if nil, one will be created)
	S3Client *s3.Client

	// DynamoDBClient for distributed locking (optional, if nil, uses S3 for locking)
	DynamoDBClient *dynamodb.Client

	// LockTableName is the DynamoDB table name for locks (required if DynamoDBClient is set)
	LockTableName string

	// CacheDir is the local directory for caching files (required)
	CacheDir string

	// CacheConfig configures caching behavior
	CacheConfig CacheConfig

	// LazyLoad enables lazy loading (only download files when accessed)
	LazyLoad bool

	// Context for AWS operations (if nil, context.Background() is used)
	Context context.Context
}

// S3Directory is a Directory implementation backed by AWS S3 with local caching.
type S3Directory struct {
	bucket         string
	prefix         string
	s3Client       *s3.Client
	dynamoClient   *dynamodb.Client
	lockTableName  string
	ctx            context.Context
	cache          *fileCache
	lockKey        string
	lockAcquired   bool
	mu             sync.RWMutex
	stats          atomic.Pointer[CacheStats]
}

// NewS3Directory creates a new S3-backed Directory with local caching.
func NewS3Directory(config S3DirectoryConfig) (*S3Directory, error) {
	if config.Bucket == "" {
		return nil, fmt.Errorf("bucket name is required")
	}

	if config.CacheDir == "" {
		return nil, fmt.Errorf("cache directory is required")
	}

	if config.S3Client == nil {
		return nil, fmt.Errorf("S3 client is required")
	}

	ctx := config.Context
	if ctx == nil {
		ctx = context.Background()
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
		bucket:        config.Bucket,
		prefix:        strings.TrimSuffix(config.Prefix, "/"),
		s3Client:      config.S3Client,
		dynamoClient:  config.DynamoDBClient,
		lockTableName: config.LockTableName,
		ctx:           ctx,
		cache:         cache,
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
	resp, err := d.s3Client.GetObject(d.ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object from S3: %w", err)
	}

	// Read into memory first (for smaller files) or cache locally
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
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
	_, err := d.s3Client.DeleteObject(d.ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
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
	_, err := d.s3Client.CopyObject(d.ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(d.bucket),
		CopySource: aws.String(path.Join(d.bucket, oldKey)),
		Key:        aws.String(newKey),
	})
	if err != nil {
		return fmt.Errorf("failed to copy object in S3: %w", err)
	}

	// Delete old object
	_, err = d.s3Client.DeleteObject(d.ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(oldKey),
	})
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

	resp, err := d.s3Client.HeadObject(d.ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to head object in S3: %w", err)
	}

	return &s3FileInfo{
		name:    filepath.Base(name),
		size:    *resp.ContentLength,
		modTime: *resp.LastModified,
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
	paginator := s3.NewListObjectsV2Paginator(d.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(d.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(d.ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects in S3: %w", err)
		}

		for _, obj := range page.Contents {
			// Skip the prefix itself if it appears as an object
			if *obj.Key == prefix {
				continue
			}

			// Extract relative name
			relName := strings.TrimPrefix(*obj.Key, prefix)

			result = append(result, &s3FileInfo{
				name:    relName,
				size:    *obj.Size,
				modTime: *obj.LastModified,
				mode:    0644,
			})
		}
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

// Lock acquires a distributed lock using DynamoDB or S3.
func (d *S3Directory) Lock() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.lockAcquired {
		return fmt.Errorf("lock already acquired")
	}

	lockKey := d.s3Key("write.lock")

	if d.dynamoClient != nil && d.lockTableName != "" {
		// Use DynamoDB for distributed locking
		return d.lockWithDynamoDB(lockKey)
	}

	// Fallback to S3-based locking (less reliable but works)
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

	if d.dynamoClient != nil && d.lockTableName != "" {
		return d.unlockWithDynamoDB(lockKey)
	}

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

// lockWithDynamoDB uses DynamoDB for distributed locking.
func (d *S3Directory) lockWithDynamoDB(lockKey string) error {
	// Try to acquire lock with conditional put
	ttl := time.Now().Add(5 * time.Minute).Unix()

	_, err := d.dynamoClient.PutItem(d.ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.lockTableName),
		Item: map[string]types.AttributeValue{
			"LockKey": &types.AttributeValueMemberS{Value: lockKey},
			"TTL":     &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", ttl)},
		},
		ConditionExpression: aws.String("attribute_not_exists(LockKey)"),
	})

	if err != nil {
		return fmt.Errorf("failed to acquire DynamoDB lock: %w", err)
	}

	d.lockKey = lockKey
	d.lockAcquired = true
	return nil
}

// unlockWithDynamoDB releases the DynamoDB lock.
func (d *S3Directory) unlockWithDynamoDB(lockKey string) error {
	_, err := d.dynamoClient.DeleteItem(d.ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(d.lockTableName),
		Key: map[string]types.AttributeValue{
			"LockKey": &types.AttributeValueMemberS{Value: lockKey},
		},
	})

	if err != nil {
		return fmt.Errorf("failed to release DynamoDB lock: %w", err)
	}

	d.lockAcquired = false
	return nil
}

// lockWithS3 uses S3 for locking (less reliable, uses conditional put).
func (d *S3Directory) lockWithS3(lockKey string) error {
	// Try to create lock file with If-None-Match header (atomic create)
	_, err := d.s3Client.PutObject(d.ctx, &s3.PutObjectInput{
		Bucket:      aws.String(d.bucket),
		Key:         aws.String(lockKey),
		Body:        bytes.NewReader([]byte("locked")),
		IfNoneMatch: aws.String("*"), // Only succeed if object doesn't exist
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
	_, err := d.s3Client.DeleteObject(d.ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(lockKey),
	})

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

	// Upload to S3
	_, err := w.dir.s3Client.PutObject(w.dir.ctx, &s3.PutObjectInput{
		Bucket: aws.String(w.dir.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(w.buf.Bytes()),
	})

	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	// Cache the file locally
	if err := w.dir.cache.put(w.name, w.buf.Bytes()); err != nil {
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
