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
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// fileCache implements an LRU cache for files.
type fileCache struct {
	cacheDir     string
	config       CacheConfig
	entries      map[string]*cacheEntry
	lru          *list.List
	mu           sync.RWMutex
	currentSize  atomic.Int64
	currentCount atomic.Int64
}

type cacheEntry struct {
	name     string
	path     string
	size     int64
	element  *list.Element
}

// newFileCache creates a new file cache.
func newFileCache(cacheDir string, config CacheConfig) (*fileCache, error) {
	// Set defaults if not specified
	if config.MaxCacheSizeBytes == 0 {
		config.MaxCacheSizeBytes = 1024 * 1024 * 1024 // 1GB default
	}
	if config.MaxCacheEntries == 0 {
		config.MaxCacheEntries = 1000 // 1000 files default
	}
	if config.EvictionPolicy == "" {
		config.EvictionPolicy = "lru"
	}

	// Create cache directory if it doesn't exist
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	return &fileCache{
		cacheDir: cacheDir,
		config:   config,
		entries:  make(map[string]*cacheEntry),
		lru:      list.New(),
	}, nil
}

// get retrieves a file from the cache. Returns the path to the cached file and true if found.
func (c *fileCache) get(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[name]
	if !ok {
		return "", false
	}

	// Move to front of LRU list (most recently used)
	c.lru.MoveToFront(entry.element)

	// Verify file still exists
	if _, err := os.Stat(entry.path); err != nil {
		// File was deleted externally, remove from cache
		delete(c.entries, name)
		c.lru.Remove(entry.element)
		c.currentSize.Add(-entry.size)
		c.currentCount.Add(-1)
		return "", false
	}

	return entry.path, true
}

// put adds or updates a file in the cache.
func (c *fileCache) put(name string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	size := int64(len(data))

	// Check if we need to evict entries
	for c.shouldEvict(size) {
		c.evictLRU()
	}

	// Remove existing entry if present
	if existing, ok := c.entries[name]; ok {
		c.lru.Remove(existing.element)
		c.currentSize.Add(-existing.size)
		c.currentCount.Add(-1)
		_ = os.Remove(existing.path)
	}

	// Write file to cache directory
	cachePath := filepath.Join(c.cacheDir, filepath.Base(name))
	if err := os.WriteFile(cachePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write cache file: %w", err)
	}

	// Add new entry
	entry := &cacheEntry{
		name: name,
		path: cachePath,
		size: size,
	}
	entry.element = c.lru.PushFront(entry)
	c.entries[name] = entry
	c.currentSize.Add(size)
	c.currentCount.Add(1)

	return nil
}

// remove removes a file from the cache.
func (c *fileCache) remove(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[name]
	if !ok {
		return
	}

	delete(c.entries, name)
	c.lru.Remove(entry.element)
	c.currentSize.Add(-entry.size)
	c.currentCount.Add(-1)
	_ = os.Remove(entry.path)
}

// rename updates the cache entry for a renamed file.
func (c *fileCache) rename(oldName, newName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[oldName]
	if !ok {
		return
	}

	// Update entry
	delete(c.entries, oldName)
	entry.name = newName
	c.entries[newName] = entry

	// Move to front of LRU
	c.lru.MoveToFront(entry.element)
}

// setConfig updates the cache configuration.
func (c *fileCache) setConfig(config CacheConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.config = config

	// Evict entries if new limits are exceeded
	for c.currentSize.Load() > config.MaxCacheSizeBytes ||
		c.currentCount.Load() > int64(config.MaxCacheEntries) {
		if !c.evictLRU() {
			break
		}
	}

	return nil
}

// shouldEvict returns true if we need to evict entries before adding new data.
func (c *fileCache) shouldEvict(newSize int64) bool {
	if c.lru.Len() == 0 {
		return false
	}

	wouldExceedSize := c.currentSize.Load()+newSize > c.config.MaxCacheSizeBytes
	wouldExceedCount := c.currentCount.Load() >= int64(c.config.MaxCacheEntries)

	return wouldExceedSize || wouldExceedCount
}

// evictLRU evicts the least recently used entry. Returns true if an entry was evicted.
func (c *fileCache) evictLRU() bool {
	if c.lru.Len() == 0 {
		return false
	}

	// Get least recently used entry (back of list)
	element := c.lru.Back()
	if element == nil {
		return false
	}

	entry := element.Value.(*cacheEntry)

	// Remove from cache
	delete(c.entries, entry.name)
	c.lru.Remove(element)
	c.currentSize.Add(-entry.size)
	c.currentCount.Add(-1)

	// Delete file
	_ = os.Remove(entry.path)

	return true
}

// clear removes all entries from the cache.
func (c *fileCache) clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for name, entry := range c.entries {
		_ = os.Remove(entry.path)
		delete(c.entries, name)
	}

	c.lru.Init()
	c.currentSize.Store(0)
	c.currentCount.Store(0)

	return nil
}

// stats returns cache statistics.
func (c *fileCache) stats() (int64, int) {
	return c.currentSize.Load(), int(c.currentCount.Load())
}
