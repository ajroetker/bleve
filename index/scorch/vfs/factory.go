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
	"strings"
)

// DirectoryType represents the type of directory implementation.
type DirectoryType string

const (
	// DirectoryTypeFS uses the local filesystem
	DirectoryTypeFS DirectoryType = "fs"

	// DirectoryTypeS3 uses S3 with local caching
	DirectoryTypeS3 DirectoryType = "s3"
)

// DirectoryConfig is a unified configuration for creating directories.
type DirectoryConfig struct {
	// Type specifies the directory implementation to use
	Type DirectoryType

	// Path is the base path (for FS) or prefix (for S3)
	Path string

	// S3Config is used when Type is DirectoryTypeS3
	S3Config *S3DirectoryConfig
}

// NewDirectory creates a new Directory based on the configuration.
func NewDirectory(config DirectoryConfig) (Directory, error) {
	switch config.Type {
	case DirectoryTypeFS:
		return NewFSDirectory(config.Path)

	case DirectoryTypeS3:
		if config.S3Config == nil {
			return nil, fmt.Errorf("S3Config is required for S3 directory type")
		}
		return NewS3Directory(*config.S3Config)

	default:
		return nil, fmt.Errorf("unknown directory type: %s", config.Type)
	}
}

// ParseDirectoryURL parses a directory URL and returns the appropriate configuration.
// Supported formats:
//   - file:///path/to/dir or /path/to/dir -> filesystem
//   - s3://bucket/prefix?region=us-east-1&cache=/tmp/cache -> S3
func ParseDirectoryURL(url string, s3Config *S3DirectoryConfig) (DirectoryConfig, error) {
	if url == "" {
		return DirectoryConfig{}, fmt.Errorf("empty URL")
	}

	// Filesystem URLs
	if strings.HasPrefix(url, "file://") {
		return DirectoryConfig{
			Type: DirectoryTypeFS,
			Path: strings.TrimPrefix(url, "file://"),
		}, nil
	}

	// S3 URLs
	if strings.HasPrefix(url, "s3://") {
		if s3Config == nil {
			return DirectoryConfig{}, fmt.Errorf("S3 configuration required for s3:// URLs")
		}

		// Parse S3 URL: s3://bucket/prefix
		url = strings.TrimPrefix(url, "s3://")
		parts := strings.SplitN(url, "/", 2)

		if len(parts) == 0 {
			return DirectoryConfig{}, fmt.Errorf("invalid S3 URL: missing bucket")
		}

		s3Config.Bucket = parts[0]
		if len(parts) > 1 {
			// Remove query parameters if present
			prefix := parts[1]
			if idx := strings.Index(prefix, "?"); idx != -1 {
				prefix = prefix[:idx]
			}
			s3Config.Prefix = prefix
		}

		return DirectoryConfig{
			Type:     DirectoryTypeS3,
			Path:     s3Config.Prefix,
			S3Config: s3Config,
		}, nil
	}

	// Default to filesystem for absolute paths
	if strings.HasPrefix(url, "/") {
		return DirectoryConfig{
			Type: DirectoryTypeFS,
			Path: url,
		}, nil
	}

	return DirectoryConfig{}, fmt.Errorf("unsupported URL format: %s", url)
}
