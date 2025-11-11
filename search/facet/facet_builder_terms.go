//  Copyright (c) 2014 Couchbase, Inc.
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

package facet

import (
	"bytes"
	"reflect"
	"regexp"
	"sort"

	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/size"
)

var reflectStaticSizeTermsFacetBuilder int

func init() {
	var tfb TermsFacetBuilder
	reflectStaticSizeTermsFacetBuilder = int(reflect.TypeOf(tfb).Size())
}

type TermsFacetBuilder struct {
	size        int
	field       string
	prefixBytes []byte
	regex       *regexp.Regexp
	termsCount  map[string]int
	total       int
	missing     int
	sawValue    bool
}

// NewTermsFacetBuilder creates a new TermsFacetBuilder for the specified field.
//
// Parameters:
//   - field: The field to facet on
//   - size: Maximum number of facet terms to return (top N by count)
//   - prefix: Optional prefix filter - only terms starting with this string are included.
//     Useful for search-as-you-type faceting. Pass empty string for no prefix filtering.
//   - pattern: Optional regex pattern - only terms matching this pattern are included.
//     Pass empty string for no pattern filtering.
//
// When both prefix and pattern are provided, terms must match both (AND logic).
// Returns an error if the regex pattern is invalid.
func NewTermsFacetBuilder(field string, size int, prefix, pattern string) (*TermsFacetBuilder, error) {
	fb := &TermsFacetBuilder{
		size:       size,
		field:      field,
		termsCount: make(map[string]int),
	}

	// Convert prefix to []byte once for zero-allocation comparisons
	if prefix != "" {
		fb.prefixBytes = []byte(prefix)
	}

	// Compile regex once
	if pattern != "" {
		var err error
		fb.regex, err = regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
	}

	return fb, nil
}

func (fb *TermsFacetBuilder) Size() int {
	sizeInBytes := reflectStaticSizeTermsFacetBuilder + size.SizeOfPtr +
		len(fb.field) +
		len(fb.prefixBytes) +
		size.SizeOfPtr // regex pointer

	for k := range fb.termsCount {
		sizeInBytes += size.SizeOfString + len(k) +
			size.SizeOfInt
	}

	return sizeInBytes
}

func (fb *TermsFacetBuilder) Field() string {
	return fb.field
}

// UpdateVisitor is called for each term in each document during search.
// It applies prefix and/or regex filtering before counting terms.
//
// The filtering uses zero-allocation techniques (bytes.HasPrefix and regexp.Match)
// to avoid string conversions for non-matching terms. This is especially beneficial
// for search-as-you-type faceting where most terms don't match the filter.
//
// Non-matching terms still contribute to the Total count but are excluded from
// the facet results and counted in Other.
func (fb *TermsFacetBuilder) UpdateVisitor(term []byte) {
	// Fast prefix check on []byte - zero allocation
	if len(fb.prefixBytes) > 0 && !bytes.HasPrefix(term, fb.prefixBytes) {
		fb.total++
		return
	}

	// Fast regex check on []byte - zero allocation
	if fb.regex != nil && !fb.regex.Match(term) {
		fb.total++
		return
	}

	// Only convert to string if term matches filters
	termStr := string(term)
	fb.sawValue = true
	fb.termsCount[termStr] = fb.termsCount[termStr] + 1
	fb.total++
}

func (fb *TermsFacetBuilder) StartDoc() {
	fb.sawValue = false
}

func (fb *TermsFacetBuilder) EndDoc() {
	if !fb.sawValue {
		fb.missing++
	}
}

func (fb *TermsFacetBuilder) Result() *search.FacetResult {
	rv := search.FacetResult{
		Field:   fb.field,
		Total:   fb.total,
		Missing: fb.missing,
	}

	rv.Terms = &search.TermFacets{}

	for term, count := range fb.termsCount {
		tf := &search.TermFacet{
			Term:  term,
			Count: count,
		}

		rv.Terms.Add(tf)
	}

	sort.Sort(rv.Terms)

	// we now have the list of the top N facets
	trimTopN := fb.size
	if trimTopN > rv.Terms.Len() {
		trimTopN = rv.Terms.Len()
	}
	rv.Terms.TrimToTopN(trimTopN)

	notOther := 0
	for _, tf := range rv.Terms.Terms() {
		notOther += tf.Count
	}
	rv.Other = fb.total - notOther

	return &rv
}
