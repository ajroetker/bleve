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
	"reflect"

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
	UpdateVisitor(term []byte)
	EndDoc()

	Result() *AggregationResult
	Field() string
	Type() string

	Size() int
}

// AggregationsBuilder manages multiple aggregation builders
type AggregationsBuilder struct {
	indexReader      index.IndexReader
	aggregationNames []string
	aggregations     []AggregationBuilder
	aggregationsByField map[string][]AggregationBuilder
	fields           []string
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
	ab.aggregationsByField[aggregationBuilder.Field()] = append(
		ab.aggregationsByField[aggregationBuilder.Field()], aggregationBuilder)
	ab.fields = append(ab.fields, aggregationBuilder.Field())
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
			aggregationBuilder.UpdateVisitor(term)
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
func (ab *AggregationsBuilder) Results() map[string]*AggregationResult {
	results := make(map[string]*AggregationResult, len(ab.aggregations))
	for i, aggregationBuilder := range ab.aggregations {
		results[ab.aggregationNames[i]] = aggregationBuilder.Result()
	}
	return results
}

// AggregationResult represents the result of an aggregation
type AggregationResult struct {
	Field string      `json:"field"`
	Type  string      `json:"type"`
	Value interface{} `json:"value"`
}

func (ar *AggregationResult) Size() int {
	sizeInBytes := reflectStaticSizeAggregationResult
	sizeInBytes += len(ar.Field)
	sizeInBytes += len(ar.Type)
	// Value size depends on type, using approximate size
	sizeInBytes += size.SizeOfFloat64
	return sizeInBytes
}
