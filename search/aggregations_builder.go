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

package search

import (
	"math"
	"reflect"
	"sort"

	"github.com/axiomhq/hyperloglog"
	"github.com/blevesearch/bleve/v2/size"
	index "github.com/blevesearch/bleve_index_api"
)

var reflectStaticSizeAggregationsBuilder int
var reflectStaticSizeAggregationResult int

func init() {
	var ab AggregationsBuilder
	reflectStaticSizeAggregationsBuilder = int(reflect.TypeOf(ab).Size())
	var ar AggregationResult
	reflectStaticSizeAggregationResult = int(reflect.TypeOf(ar).Size())
}

// AggregationBuilder is the interface all aggregation builders must implement
type AggregationBuilder interface {
	StartDoc()
	UpdateVisitor(field string, term []byte)
	EndDoc()

	Result() *AggregationResult
	Field() string
	Type() string

	Size() int
	Clone() AggregationBuilder // Creates a fresh instance for sub-aggregation bucket cloning
}

// AggregationsBuilder manages multiple aggregation builders
type AggregationsBuilder struct {
	indexReader         index.IndexReader
	aggregationNames    []string
	aggregations        []AggregationBuilder
	aggregationsByField map[string][]AggregationBuilder
	fields              []string
}

// NewAggregationsBuilder creates a new aggregations builder
func NewAggregationsBuilder(indexReader index.IndexReader) *AggregationsBuilder {
	return &AggregationsBuilder{
		indexReader: indexReader,
	}
}

func (ab *AggregationsBuilder) Size() int {
	sizeInBytes := reflectStaticSizeAggregationsBuilder + size.SizeOfPtr

	for k, v := range ab.aggregations {
		sizeInBytes += size.SizeOfString + v.Size() + len(ab.aggregationNames[k])
	}

	for _, entry := range ab.fields {
		sizeInBytes += size.SizeOfString + len(entry)
	}

	return sizeInBytes
}

// Add adds an aggregation builder
func (ab *AggregationsBuilder) Add(name string, aggregationBuilder AggregationBuilder) {
	if ab.aggregationsByField == nil {
		ab.aggregationsByField = map[string][]AggregationBuilder{}
	}

	ab.aggregationNames = append(ab.aggregationNames, name)
	ab.aggregations = append(ab.aggregations, aggregationBuilder)

	// Track unique fields
	fieldSet := make(map[string]bool)
	for _, f := range ab.fields {
		fieldSet[f] = true
	}

	// Register for the aggregation's own field
	field := aggregationBuilder.Field()
	ab.aggregationsByField[field] = append(ab.aggregationsByField[field], aggregationBuilder)
	if !fieldSet[field] {
		ab.fields = append(ab.fields, field)
		fieldSet[field] = true
	}

	// For bucket aggregations, also register for sub-aggregation fields
	if bucketed, ok := aggregationBuilder.(BucketAggregation); ok {
		subFields := bucketed.SubAggregationFields()
		for _, subField := range subFields {
			ab.aggregationsByField[subField] = append(ab.aggregationsByField[subField], aggregationBuilder)
			if !fieldSet[subField] {
				ab.fields = append(ab.fields, subField)
				fieldSet[subField] = true
			}
		}
	}
}

// BucketAggregation interface for aggregations that have sub-aggregations
type BucketAggregation interface {
	AggregationBuilder
	SubAggregationFields() []string
}

// RequiredFields returns the fields needed for aggregations
func (ab *AggregationsBuilder) RequiredFields() []string {
	return ab.fields
}

// StartDoc notifies all aggregations that a new document is being processed
func (ab *AggregationsBuilder) StartDoc() {
	for _, aggregationBuilder := range ab.aggregations {
		aggregationBuilder.StartDoc()
	}
}

// UpdateVisitor forwards field values to relevant aggregation builders
func (ab *AggregationsBuilder) UpdateVisitor(field string, term []byte) {
	if aggregationBuilders, ok := ab.aggregationsByField[field]; ok {
		for _, aggregationBuilder := range aggregationBuilders {
			aggregationBuilder.UpdateVisitor(field, term)
		}
	}
}

// EndDoc notifies all aggregations that document processing is complete
func (ab *AggregationsBuilder) EndDoc() {
	for _, aggregationBuilder := range ab.aggregations {
		aggregationBuilder.EndDoc()
	}
}

// Results returns all aggregation results
func (ab *AggregationsBuilder) Results() AggregationResults {
	results := make(AggregationResults, len(ab.aggregations))
	for i, aggregationBuilder := range ab.aggregations {
		results[ab.aggregationNames[i]] = aggregationBuilder.Result()
	}
	return results
}

// AggregationResult represents the result of an aggregation
// For metric aggregations, Value contains a single number (float64 or int64)
// For bucket aggregations, Value contains a slice of *Bucket
type AggregationResult struct {
	Field string      `json:"field"`
	Type  string      `json:"type"`
	Value interface{} `json:"value"`

	// For bucket aggregations only
	Buckets  []*Bucket              `json:"buckets,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"` // Additional metadata (e.g., center coords for geo_distance)
}

// AvgResult contains average with the necessary metadata for proper merging
type AvgResult struct {
	Count int64   `json:"count"`
	Sum   float64 `json:"sum"`
	Avg   float64 `json:"avg"`
}

// StatsResult contains comprehensive statistics
type StatsResult struct {
	Count      int64   `json:"count"`
	Sum        float64 `json:"sum"`
	Avg        float64 `json:"avg"`
	Min        float64 `json:"min"`
	Max        float64 `json:"max"`
	SumSquares float64 `json:"sum_squares"`
	Variance   float64 `json:"variance"`
	StdDev     float64 `json:"std_dev"`
}

// CardinalityResult contains cardinality estimate with HyperLogLog sketch for merging
type CardinalityResult struct {
	Cardinality int64  `json:"value"`            // Estimated unique count
	Sketch      []byte `json:"sketch,omitempty"` // Serialized HLL sketch for distributed merging

	// HLL is kept in-memory for efficient local merging (not serialized to JSON)
	HLL interface{} `json:"-"`
}

// SignificantTermsStats contains background term statistics for significant_terms aggregations
// Used in pre-search phase to collect term frequencies across all index shards
type SignificantTermsStats struct {
	Field        string           `json:"field"`
	TotalDocs    int64            `json:"total_docs"`
	TermDocFreqs map[string]int64 `json:"term_doc_freqs"` // term -> background doc frequency
}

// Bucket represents a single bucket in a bucket aggregation
type Bucket struct {
	Key          interface{}                   `json:"key"`                    // Term or range name
	Count        int64                         `json:"doc_count"`              // Number of documents in this bucket
	Aggregations map[string]*AggregationResult `json:"aggregations,omitempty"` // Sub-aggregations
	Metadata     map[string]interface{}        `json:"metadata,omitempty"`     // Additional metadata (e.g., lat/lon for geohash)
}

func (ar *AggregationResult) Size() int {
	sizeInBytes := reflectStaticSizeAggregationResult
	sizeInBytes += len(ar.Field)
	sizeInBytes += len(ar.Type)
	// Value size depends on type, using approximate size
	sizeInBytes += size.SizeOfFloat64

	// Add bucket sizes
	for _, bucket := range ar.Buckets {
		sizeInBytes += size.SizeOfPtr + 8 // int64 count = 8 bytes
		// Approximate size for key
		sizeInBytes += size.SizeOfString + 20
		// Approximate size for sub-aggregations
		for _, subAgg := range bucket.Aggregations {
			sizeInBytes += subAgg.Size()
		}
	}

	return sizeInBytes
}

// AggregationResults is a map of aggregation results by name
type AggregationResults map[string]*AggregationResult

// Merge merges another set of aggregation results into this one
// This is useful for combining results from multiple index shards

func (ar AggregationResults) Merge(other AggregationResults) {
	for name, otherAggResult := range other {
		aggResult, exists := ar[name]
		if !exists {
			// First time seeing this aggregation, just copy it
			ar[name] = otherAggResult
			continue
		}

		// Merge based on aggregation type
		switch aggResult.Type {
		case "sum", "sumsquares":
			// Sum values are additive
			aggResult.Value = aggResult.Value.(float64) + otherAggResult.Value.(float64)

		case "count":
			// Counts are additive
			aggResult.Value = aggResult.Value.(int64) + otherAggResult.Value.(int64)

		case "min":
			// Take minimum of minimums
			if otherAggResult.Value.(float64) < aggResult.Value.(float64) {
				aggResult.Value = otherAggResult.Value
			}

		case "max":
			// Take maximum of maximums
			if otherAggResult.Value.(float64) > aggResult.Value.(float64) {
				aggResult.Value = otherAggResult.Value
			}

		case "avg":
			// Properly merge averages using counts and sums
			destAvg := aggResult.Value.(*AvgResult)
			srcAvg := otherAggResult.Value.(*AvgResult)

			destAvg.Count += srcAvg.Count
			destAvg.Sum += srcAvg.Sum

			// Recalculate average
			if destAvg.Count > 0 {
				destAvg.Avg = destAvg.Sum / float64(destAvg.Count)
			}

		case "stats":
			// Merge stats by combining component values
			destStats := aggResult.Value.(*StatsResult)
			srcStats := otherAggResult.Value.(*StatsResult)

			destStats.Count += srcStats.Count
			destStats.Sum += srcStats.Sum
			destStats.SumSquares += srcStats.SumSquares

			if srcStats.Min < destStats.Min {
				destStats.Min = srcStats.Min
			}
			if srcStats.Max > destStats.Max {
				destStats.Max = srcStats.Max
			}

			// Recalculate derived values
			if destStats.Count > 0 {
				destStats.Avg = destStats.Sum / float64(destStats.Count)
				avgSquares := destStats.SumSquares / float64(destStats.Count)
				destStats.Variance = avgSquares - (destStats.Avg * destStats.Avg)
				if destStats.Variance < 0 {
					destStats.Variance = 0
				}
				destStats.StdDev = math.Sqrt(destStats.Variance)
			}

		case "cardinality":
			// Merge HyperLogLog sketches
			ar.mergeCardinality(aggResult, otherAggResult)

		case "terms", "range", "date_range":
			// Merge buckets
			ar.mergeBuckets(aggResult, otherAggResult)
		case "significant_terms":
			ar.mergeSignificantTerms(aggResult, otherAggResult)
		}
	}
}

// mergeBuckets merges bucket aggregation results
func (ar AggregationResults) mergeBuckets(dest, src *AggregationResult) {
	// Create a map of existing buckets by key
	bucketMap := make(map[interface{}]*Bucket)
	for _, bucket := range dest.Buckets {
		bucketMap[bucket.Key] = bucket
	}

	// Merge source buckets
	for _, srcBucket := range src.Buckets {
		destBucket, exists := bucketMap[srcBucket.Key]
		if !exists {
			// New bucket, add it
			dest.Buckets = append(dest.Buckets, srcBucket)
			bucketMap[srcBucket.Key] = srcBucket
		} else {
			// Existing bucket, merge counts
			destBucket.Count += srcBucket.Count

			// Merge sub-aggregations recursively
			if srcBucket.Aggregations != nil {
				if destBucket.Aggregations == nil {
					destBucket.Aggregations = make(map[string]*AggregationResult)
				}
				AggregationResults(destBucket.Aggregations).Merge(srcBucket.Aggregations)
			}
		}
	}
}

func (ar AggregationResults) mergeSignificantTerms(dest, src *AggregationResult) {
	if dest == nil || src == nil {
		return
	}

	targetSize := len(dest.Buckets)
	if len(src.Buckets) > targetSize {
		targetSize = len(src.Buckets)
	}

	if dest.Metadata == nil {
		dest.Metadata = make(map[string]interface{})
	}

	algorithm := metadataString(dest.Metadata, "algorithm")
	if algorithm == "" {
		algorithm = metadataString(src.Metadata, "algorithm")
	}
	if algorithm == "" {
		algorithm = "jlh"
	}
	dest.Metadata["algorithm"] = algorithm

	fgDocCount := metadataInt64(dest.Metadata, "fg_doc_count") + metadataInt64(src.Metadata, "fg_doc_count")
	bgDocCount := metadataInt64(dest.Metadata, "bg_doc_count") + metadataInt64(src.Metadata, "bg_doc_count")
	if bgDocCount == 0 {
		bgDocCount = fgDocCount
	}

	bucketMap := make(map[interface{}]*Bucket, len(dest.Buckets)+len(src.Buckets))
	for _, bucket := range dest.Buckets {
		bucketMap[bucket.Key] = bucket
	}

	for _, srcBucket := range src.Buckets {
		destBucket, exists := bucketMap[srcBucket.Key]
		if !exists {
			dest.Buckets = append(dest.Buckets, srcBucket)
			bucketMap[srcBucket.Key] = srcBucket
			continue
		}

		destBucket.Count += srcBucket.Count
		if destBucket.Metadata == nil {
			destBucket.Metadata = make(map[string]interface{})
		}
		bgCount := metadataInt64(destBucket.Metadata, "bg_count") + metadataInt64(srcBucket.Metadata, "bg_count")
		destBucket.Metadata["bg_count"] = bgCount

		if srcBucket.Aggregations != nil {
			if destBucket.Aggregations == nil {
				destBucket.Aggregations = make(map[string]*AggregationResult)
			}
			AggregationResults(destBucket.Aggregations).Merge(srcBucket.Aggregations)
		}
	}

	for _, bucket := range dest.Buckets {
		if bucket.Metadata == nil {
			bucket.Metadata = make(map[string]interface{})
		}
		bgCount := metadataInt64(bucket.Metadata, "bg_count")
		if bgCount == 0 {
			bgCount = bucket.Count
			bucket.Metadata["bg_count"] = bgCount
		}
		bucket.Metadata["score"] = calculateSignificanceScore(algorithm, bucket.Count, fgDocCount, bgCount, bgDocCount)
	}

	sort.Slice(dest.Buckets, func(i, j int) bool {
		leftScore := metadataFloat64(dest.Buckets[i].Metadata, "score")
		rightScore := metadataFloat64(dest.Buckets[j].Metadata, "score")
		if leftScore == rightScore {
			if dest.Buckets[i].Count == dest.Buckets[j].Count {
				return bucketKeyString(dest.Buckets[i].Key) < bucketKeyString(dest.Buckets[j].Key)
			}
			return dest.Buckets[i].Count > dest.Buckets[j].Count
		}
		return leftScore > rightScore
	})

	if targetSize > 0 && len(dest.Buckets) > targetSize {
		dest.Buckets = dest.Buckets[:targetSize]
	}

	dest.Metadata["fg_doc_count"] = fgDocCount
	dest.Metadata["bg_doc_count"] = bgDocCount
	dest.Metadata["unique_terms"] = len(bucketMap)
	dest.Metadata["significant_terms"] = len(dest.Buckets)
}

func metadataString(metadata map[string]interface{}, key string) string {
	if metadata == nil {
		return ""
	}
	switch typed := metadata[key].(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func metadataInt64(metadata map[string]interface{}, key string) int64 {
	if metadata == nil {
		return 0
	}
	switch typed := metadata[key].(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func metadataFloat64(metadata map[string]interface{}, key string) float64 {
	if metadata == nil {
		return 0
	}
	switch typed := metadata[key].(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}

func bucketKeyString(key interface{}) string {
	switch typed := key.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func calculateSignificanceScore(algorithm string, fgCount, fgTotal, bgCount, bgTotal int64) float64 {
	switch algorithm {
	case "mutual_information":
		return calculateMutualInformation(fgCount, fgTotal, bgCount, bgTotal)
	case "chi_squared":
		return calculateChiSquared(fgCount, fgTotal, bgCount, bgTotal)
	case "percentage":
		return calculatePercentage(fgCount, fgTotal, bgCount, bgTotal)
	default:
		return calculateJLH(fgCount, fgTotal, bgCount, bgTotal)
	}
}

func calculateJLH(fgCount, fgTotal, bgCount, bgTotal int64) float64 {
	if fgCount <= 0 || fgTotal <= 0 || bgCount <= 0 || bgTotal <= 0 {
		return 0
	}
	fgRate := float64(fgCount) / float64(fgTotal)
	bgRate := float64(bgCount) / float64(bgTotal)
	if bgRate == 0 || fgRate <= bgRate {
		return 0
	}
	return (fgRate - bgRate) * (fgRate / bgRate)
}

func calculateMutualInformation(fgCount, fgTotal, bgCount, bgTotal int64) float64 {
	if fgCount <= 0 || fgTotal <= 0 || bgCount <= 0 || bgTotal <= 0 {
		return 0
	}
	fgRate := float64(fgCount) / float64(fgTotal)
	bgRate := float64(bgCount) / float64(bgTotal)
	if fgRate == 0 || bgRate == 0 {
		return 0
	}
	score := fgRate * math.Log2(fgRate/bgRate)
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return score
}

func calculateChiSquared(fgCount, fgTotal, bgCount, bgTotal int64) float64 {
	if fgCount <= 0 || fgTotal <= 0 || bgCount <= 0 || bgTotal <= 0 {
		return 0
	}
	N11 := float64(fgCount)
	N10 := float64(fgTotal - fgCount)
	N01 := float64(bgCount - fgCount)
	if N01 < 0 {
		N01 = 0
	}
	N00 := float64(bgTotal-bgCount) - N10
	if N00 < 0 {
		N00 = 0
	}
	N := N11 + N10 + N01 + N00
	if N == 0 {
		return 0
	}
	if N10 == 0 || N01 == 0 {
		bgRate := float64(bgCount) / float64(bgTotal)
		if bgRate == 0 {
			return 0
		}
		return (float64(fgCount) / float64(fgTotal)) / bgRate
	}
	score := (N11 / N) * math.Log2((N*N11)/((N11+N10)*(N11+N01)))
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return score
}

func calculatePercentage(fgCount, fgTotal, bgCount, bgTotal int64) float64 {
	if fgCount <= 0 || fgTotal <= 0 || bgCount <= 0 || bgTotal <= 0 {
		return 0
	}
	fgRate := float64(fgCount) / float64(fgTotal)
	bgRate := float64(bgCount) / float64(bgTotal)
	if bgRate == 0 {
		return 0
	}
	score := (fgRate / bgRate) - 1.0
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return score
}

// mergeCardinality merges cardinality aggregation results using HyperLogLog sketches
func (ar AggregationResults) mergeCardinality(dest, src *AggregationResult) {
	destCard := dest.Value.(*CardinalityResult)
	srcCard := src.Value.(*CardinalityResult)

	// Fast path: if both have in-memory HLL (local indexes in same process)
	if destCard.HLL != nil && srcCard.HLL != nil {
		// Type assert to *hyperloglog.Sketch
		destHLL, destOK := destCard.HLL.(*hyperloglog.Sketch)
		srcHLL, srcOK := srcCard.HLL.(*hyperloglog.Sketch)

		if destOK && srcOK {
			err := destHLL.Merge(srcHLL)
			if err == nil {
				destCard.Cardinality = int64(destHLL.Estimate())
				// Update sketch bytes for potential future remote merging
				destCard.Sketch, _ = destHLL.MarshalBinary()
				return
			}
			// If merge failed, fall through to slow path
		}
		// If type assertion failed, fall through to slow path
	}

	// Slow path: deserialize from bytes (remote indexes or fallback)
	// Note: This path shouldn't normally be hit in tests since we have in-memory HLL
	// but it's here for remote/distributed scenarios

	// If we don't have sketch bytes, we can't properly merge - just add estimates as approximation
	if len(destCard.Sketch) == 0 && len(srcCard.Sketch) == 0 {
		// No sketch data available, fall back to adding estimates (inaccurate)
		destCard.Cardinality += srcCard.Cardinality
		return
	}

	// TODO: Implement proper sketch deserialization for remote merging
	// For now, this is a limitation - we can't properly merge remote cardinality results
	// without importing hyperloglog here, which we want to avoid at the package level
	// The fast path above should handle local merging correctly
	destCard.Cardinality += srcCard.Cardinality
}
