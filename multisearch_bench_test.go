//  Copyright (c) 2024 Couchbase, Inc.
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

package bleve

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"testing"

	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	index "github.com/blevesearch/bleve_index_api"
)

// createBM25IndexMapping returns an index mapping with BM25 scoring enabled.
func createBM25IndexMapping() *mapping.IndexMappingImpl {
	fm := mapping.NewTextFieldMapping()
	fm.Store = false

	dm := mapping.NewDocumentMapping()
	dm.AddFieldMappingsAt("title", fm)
	dm.AddFieldMappingsAt("body", fm)

	im := mapping.NewIndexMapping()
	im.DefaultMapping = dm
	im.ScoringModel = index.BM25Scoring

	return im
}

// words used to generate synthetic documents
var benchWords = []string{
	"hotel", "restaurant", "airport", "museum", "station",
	"plaza", "garden", "palace", "bridge", "tower",
	"market", "library", "theater", "harbor", "castle",
	"beach", "valley", "mountain", "river", "forest",
}

// generateDoc creates a synthetic document with a mix of words.
func generateDoc(rng *rand.Rand, id int) map[string]interface{} {
	titleLen := 3 + rng.Intn(5)
	bodyLen := 20 + rng.Intn(80)

	title := ""
	for j := 0; j < titleLen; j++ {
		if j > 0 {
			title += " "
		}
		title += benchWords[rng.Intn(len(benchWords))]
	}

	body := ""
	for j := 0; j < bodyLen; j++ {
		if j > 0 {
			body += " "
		}
		body += benchWords[rng.Intn(len(benchWords))]
	}

	return map[string]interface{}{
		"title": title,
		"body":  body,
	}
}

// setupBM25Indexes creates numIndexes separate indexes, each containing
// docsPerIndex documents, and returns them along with a cleanup function.
func setupBM25Indexes(b *testing.B, numIndexes, docsPerIndex int) ([]Index, func()) {
	b.Helper()

	rng := rand.New(rand.NewSource(42))
	im := createBM25IndexMapping()

	indexes := make([]Index, numIndexes)
	paths := make([]string, numIndexes)

	for i := 0; i < numIndexes; i++ {
		path := createTmpIndexPath(b)
		paths[i] = path

		idx, err := NewUsing(path, im, Config.DefaultIndexType, Config.DefaultMemKVStore, nil)
		if err != nil {
			b.Fatalf("creating index %d: %v", i, err)
		}
		indexes[i] = idx

		batch := idx.NewBatch()
		for d := 0; d < docsPerIndex; d++ {
			docID := fmt.Sprintf("idx%d-doc%d", i, d)
			doc := generateDoc(rng, d)
			if err := batch.Index(docID, doc); err != nil {
				b.Fatalf("indexing doc: %v", err)
			}
			// flush batch every 500 docs to keep memory bounded
			if (d+1)%500 == 0 {
				if err := idx.Batch(batch); err != nil {
					b.Fatalf("flushing batch: %v", err)
				}
				batch = idx.NewBatch()
			}
		}
		// flush remaining
		if err := idx.Batch(batch); err != nil {
			b.Fatalf("flushing final batch: %v", err)
		}
	}

	cleanup := func() {
		for _, idx := range indexes {
			_ = idx.Close()
		}
		for _, p := range paths {
			cleanupTmpIndexPath(b, p)
		}
	}

	return indexes, cleanup
}

// BenchmarkMultiSearchBM25 measures the full two-phase BM25 multi-search
// across varying numbers of shards.
func BenchmarkMultiSearchBM25(b *testing.B) {
	for _, numShards := range []int{2, 4, 8} {
		b.Run(fmt.Sprintf("shards=%d", numShards), func(b *testing.B) {
			indexes, cleanup := setupBM25Indexes(b, numShards, 1000)
			defer cleanup()

			im := createBM25IndexMapping()
			alias := NewIndexAlias(indexes...)
			if err := alias.SetIndexMapping(im); err != nil {
				b.Fatal(err)
			}

			q := NewMatchQuery("hotel museum")
			q.SetField("body")
			req := NewSearchRequestOptions(q, 10, 0, false)

			ctx := context.WithValue(context.Background(),
				search.SearchTypeKey, search.GlobalScoring)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := alias.SearchInContext(ctx, req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMultiSearchBM25DeepPage measures the overhead of deep pagination
// in multi-search, which exercises the merge/sort path.
func BenchmarkMultiSearchBM25DeepPage(b *testing.B) {
	for _, from := range []int{0, 100, 500} {
		b.Run(fmt.Sprintf("from=%d", from), func(b *testing.B) {
			indexes, cleanup := setupBM25Indexes(b, 4, 1000)
			defer cleanup()

			im := createBM25IndexMapping()
			alias := NewIndexAlias(indexes...)
			if err := alias.SetIndexMapping(im); err != nil {
				b.Fatal(err)
			}

			q := NewMatchQuery("hotel")
			q.SetField("body")
			req := NewSearchRequestOptions(q, 10, from, false)

			ctx := context.WithValue(context.Background(),
				search.SearchTypeKey, search.GlobalScoring)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := alias.SearchInContext(ctx, req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMultiSearchBM25PreSearchOverhead isolates the pre-search cost
// by comparing BM25 (which requires pre-search) against TF-IDF (which doesn't).
func BenchmarkMultiSearchBM25PreSearchOverhead(b *testing.B) {
	for _, model := range []string{"bm25", "tfidf"} {
		b.Run(fmt.Sprintf("model=%s", model), func(b *testing.B) {
			rng := rand.New(rand.NewSource(42))
			numShards := 4
			docsPerShard := 1000

			im := mapping.NewIndexMapping()
			fm := mapping.NewTextFieldMapping()
			fm.Store = false
			dm := mapping.NewDocumentMapping()
			dm.AddFieldMappingsAt("title", fm)
			dm.AddFieldMappingsAt("body", fm)
			im.DefaultMapping = dm
			if model == "bm25" {
				im.ScoringModel = index.BM25Scoring
			}

			indexes := make([]Index, numShards)
			paths := make([]string, numShards)
			for i := 0; i < numShards; i++ {
				path := createTmpIndexPath(b)
				paths[i] = path
				idx, err := NewUsing(path, im, Config.DefaultIndexType, Config.DefaultMemKVStore, nil)
				if err != nil {
					b.Fatal(err)
				}
				indexes[i] = idx

				batch := idx.NewBatch()
				for d := 0; d < docsPerShard; d++ {
					if err := batch.Index(strconv.Itoa(i*docsPerShard+d), generateDoc(rng, d)); err != nil {
						b.Fatal(err)
					}
					if (d+1)%500 == 0 {
						if err := idx.Batch(batch); err != nil {
							b.Fatal(err)
						}
						batch = idx.NewBatch()
					}
				}
				if err := idx.Batch(batch); err != nil {
					b.Fatal(err)
				}
			}

			defer func() {
				for _, idx := range indexes {
					_ = idx.Close()
				}
				for _, p := range paths {
					cleanupTmpIndexPath(b, p)
				}
			}()

			alias := NewIndexAlias(indexes...)
			if err := alias.SetIndexMapping(im); err != nil {
				b.Fatal(err)
			}

			q := NewMatchQuery("hotel museum")
			q.SetField("body")
			req := NewSearchRequestOptions(q, 10, 0, false)

			ctx := context.Background()
			if model == "bm25" {
				ctx = context.WithValue(ctx, search.SearchTypeKey, search.GlobalScoring)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := alias.SearchInContext(ctx, req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMultiSearchMergeSort measures the hit sorting/merging step
// by using varying result set sizes.
func BenchmarkMultiSearchMergeSort(b *testing.B) {
	for _, size := range []int{10, 100, 500} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			indexes, cleanup := setupBM25Indexes(b, 4, 2000)
			defer cleanup()

			im := createBM25IndexMapping()
			alias := NewIndexAlias(indexes...)
			if err := alias.SetIndexMapping(im); err != nil {
				b.Fatal(err)
			}

			// use a broad query to get many hits
			q := NewMatchQuery("hotel")
			q.SetField("body")
			req := NewSearchRequestOptions(q, size, 0, false)

			ctx := context.WithValue(context.Background(),
				search.SearchTypeKey, search.GlobalScoring)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := alias.SearchInContext(ctx, req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
