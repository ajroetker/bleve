# Bleve Aggregations Guide

Bleve now supports both metric and bucket aggregations, including nested sub-aggregations within buckets!

## Quick Start

### Simple Metric Aggregations

```go
query := bleve.NewMatchAllQuery()
searchRequest := bleve.NewSearchRequest(query)

searchRequest.Aggregations = bleve.AggregationsRequest{
    "total_revenue": bleve.NewAggregationRequest("sum", "price"),
    "avg_rating":    bleve.NewAggregationRequest("avg", "rating"),
    "min_price":     bleve.NewAggregationRequest("min", "price"),
    "max_price":     bleve.NewAggregationRequest("max", "price"),
}

results, _ := index.Search(searchRequest)

// Access results
totalRevenue := results.Aggregations["total_revenue"].Value.(float64)
avgRating := results.Aggregations["avg_rating"].Value.(float64)
```

## Bucket Aggregations with Sub-Aggregations

The real power comes from bucket aggregations, which group documents and compute metrics per group.

### Terms Aggregation (Group by Field Value)

```go
// "Show me average price per brand"
query := bleve.NewMatchAllQuery()
searchRequest := bleve.NewSearchRequest(query)

// Create terms aggregation
byBrand := bleve.NewTermsAggregation("brand", 10) // top 10 brands
byBrand.AddSubAggregation("avg_price", bleve.NewAggregationRequest("avg", "price"))
byBrand.AddSubAggregation("total_sales", bleve.NewAggregationRequest("sum", "price"))
byBrand.AddSubAggregation("product_count", bleve.NewAggregationRequest("count", "price"))

searchRequest.Aggregations = bleve.AggregationsRequest{
    "by_brand": byBrand,
}

results, _ := index.Search(searchRequest)

// Access bucket results
byBrandAgg := results.Aggregations["by_brand"]
for _, bucket := range byBrandAgg.Buckets {
    brand := bucket.Key.(string)
    docCount := bucket.Count
    avgPrice := bucket.Aggregations["avg_price"].Value.(float64)
    totalSales := bucket.Aggregations["total_sales"].Value.(float64)

    fmt.Printf("Brand: %s, Products: %d, Avg Price: $%.2f, Total Sales: $%.2f\n",
        brand, docCount, avgPrice, totalSales)
}
```

**Output:**
```
Brand: Apple, Products: 15, Avg Price: $1099.99, Total Sales: $16499.85
Brand: Samsung, Products: 23, Avg Price: $799.99, Total Sales: $18399.77
Brand: Google, Products: 8, Avg Price: $699.99, Total Sales: $5599.92
```

### Range Aggregation (Group by Ranges)

```go
// "Show me products by price category"
query := bleve.NewMatchAllQuery()
searchRequest := bleve.NewSearchRequest(query)

// Define price ranges
low := 500.0
mid := 1000.0
high := 1500.0

ranges := []*bleve.numericRange{
    {Name: "budget", Min: nil, Max: &mid},           // < $1000
    {Name: "mid-range", Min: &mid, Max: &high},      // $1000-$1500
    {Name: "premium", Min: &high, Max: nil},         // > $1500
}

priceRanges := bleve.NewRangeAggregation("price", ranges)
priceRanges.AddSubAggregation("avg_rating", bleve.NewAggregationRequest("avg", "rating"))
priceRanges.AddSubAggregation("total_sold", bleve.NewAggregationRequest("sum", "units_sold"))

searchRequest.Aggregations = bleve.AggregationsRequest{
    "by_price_range": priceRanges,
}

results, _ := index.Search(searchRequest)

// Access results
byPriceRange := results.Aggregations["by_price_range"]
for _, bucket := range byPriceRange.Buckets {
    rangeName := bucket.Key.(string)
    docCount := bucket.Count
    avgRating := bucket.Aggregations["avg_rating"].Value.(float64)

    fmt.Printf("%s: %d products, avg rating: %.1f\n",
        rangeName, docCount, avgRating)
}
```

**Output:**
```
budget: 234 products, avg rating: 4.2
mid-range: 156 products, avg rating: 4.5
premium: 45 products, avg rating: 4.8
```

## Advanced: Multiple Levels of Nesting

You can nest bucket aggregations within other bucket aggregations!

```go
// "Show me sales by region, then by product category, with metrics"
query := bleve.NewMatchAllQuery()
searchRequest := bleve.NewSearchRequest(query)

// Create nested structure: regions -> categories -> metrics
byRegion := bleve.NewTermsAggregation("region", 10)

// For each region, group by category
byCategory := bleve.NewTermsAggregation("category", 20)
byCategory.AddSubAggregation("total_revenue", bleve.NewAggregationRequest("sum", "price"))
byCategory.AddSubAggregation("avg_price", bleve.NewAggregationRequest("avg", "price"))

byRegion.AddSubAggregation("by_category", byCategory)

searchRequest.Aggregations = bleve.AggregationsRequest{
    "by_region": byRegion,
}

results, _ := index.Search(searchRequest)

// Access nested results
for _, regionBucket := range results.Aggregations["by_region"].Buckets {
    region := regionBucket.Key.(string)
    fmt.Printf("\nRegion: %s (%d products)\n", region, regionBucket.Count)

    categoryAgg := regionBucket.Aggregations["by_category"]
    for _, categoryBucket := range categoryAgg.Buckets {
        category := categoryBucket.Key.(string)
        revenue := categoryBucket.Aggregations["total_revenue"].Value.(float64)
        avgPrice := categoryBucket.Aggregations["avg_price"].Value.(float64)

        fmt.Printf("  %s: $%.2f revenue, $%.2f avg price\n",
            category, revenue, avgPrice)
    }
}
```

## Aggregation Types

### Metric Aggregations
- **sum**: Sum of all values
- **avg**: Average of all values
- **min**: Minimum value
- **max**: Maximum value
- **count**: Count of values
- **sumsquares**: Sum of squares (for variance calculations)
- **stats**: All of the above plus variance and standard deviation

### Bucket Aggregations
- **terms**: Group by unique field values (like SQL GROUP BY)
- **range**: Group by numeric ranges
- **date_range**: Group by date ranges (coming soon)

## Query Filtering

All aggregations respect the query filter - they only aggregate matching documents!

```go
// "Show me average price by brand, but only for products with rating > 4.0"
query := bleve.NewNumericRangeQuery(Float64Ptr(4.0), nil)
query.SetField("rating")

searchRequest := bleve.NewSearchRequest(query)

byBrand := bleve.NewTermsAggregation("brand", 10)
byBrand.AddSubAggregation("avg_price", bleve.NewAggregationRequest("avg", "price"))

searchRequest.Aggregations = bleve.AggregationsRequest{
    "by_brand": byBrand,
}

// Only products with rating > 4.0 are aggregated
results, _ := index.Search(searchRequest)
```

## Facets vs Aggregations

**Both APIs are supported!** Use whichever fits your use case:

### Facets (Original API)
- Best for simple bucketing/counting
- No sub-aggregations
- Established API

```go
facetRequest := bleve.NewFacetRequest("price", 10)
facetRequest.AddNumericRange("cheap", nil, Float64Ptr(500.0))
facetRequest.AddNumericRange("expensive", Float64Ptr(500.0), nil)

searchRequest.Facets = bleve.FacetsRequest{
    "price_ranges": facetRequest,
}
```

### Aggregations (New API)
- Supports both metrics and buckets
- Supports sub-aggregations (nest aggregations within buckets)
- More powerful for analytics

```go
ranges := []*bleve.numericRange{
    {Name: "cheap", Min: nil, Max: Float64Ptr(500.0)},
    {Name: "expensive", Min: Float64Ptr(500.0), Max: nil},
}

priceRanges := bleve.NewRangeAggregation("price", ranges)
priceRanges.AddSubAggregation("avg_rating", bleve.NewAggregationRequest("avg", "rating"))

searchRequest.Aggregations = bleve.AggregationsRequest{
    "price_ranges": priceRanges,
}
```

## Performance

Aggregations are computed during query execution using a visitor pattern:
- ✅ Minimal overhead (piggybacks on existing field value visits)
- ✅ Only processes matching documents
- ✅ Streaming computation (constant memory per aggregation)
- ✅ Segment-level caching infrastructure for repeated queries

## JSON API

Aggregations work seamlessly with JSON requests:

```json
{
  "query": {"match_all": {}},
  "size": 0,
  "aggregations": {
    "by_brand": {
      "type": "terms",
      "field": "brand",
      "size": 10,
      "aggregations": {
        "avg_price": {
          "type": "avg",
          "field": "price"
        },
        "total_revenue": {
          "type": "sum",
          "field": "price"
        }
      }
    }
  }
}
```

Response:
```json
{
  "aggregations": {
    "by_brand": {
      "field": "brand",
      "type": "terms",
      "buckets": [
        {
          "key": "Apple",
          "doc_count": 15,
          "aggregations": {
            "avg_price": {"value": 1099.99},
            "total_revenue": {"value": 16499.85}
          }
        }
      ]
    }
  }
}
```
