// Package vectors stores and searches embeddings in an Amazon S3 Vectors
// index. Payloads stay in the document store; the index only carries keys
// and small filterable metadata.
package vectors

import (
	"context"
	"fmt"
	"strings"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors/document"
	"github.com/aws/aws-sdk-go-v2/service/s3vectors/types"
)

const (
	maxBatch    = 500
	maxGetBatch = 100
	// MaxTopK is the largest result count a single query may ask for.
	MaxTopK = 30
)

// Embedder turns text into vectors of a fixed dimension.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
}

// Index is one S3 Vectors index paired with the embedder that feeds it.
type Index struct {
	client *s3vectors.Client
	arn    string
	embed  Embedder
	metric types.DistanceMetric
}

// Item is one text to index under Key with filterable Metadata.
type Item struct {
	Key      string
	Text     string
	Metadata map[string]any
}

// Match is one query hit; a smaller Distance is a closer match.
type Match struct {
	Key      string
	Distance float32
	Metadata map[string]any
}

// Open connects to the index at indexARN and verifies its dimension matches
// the embedder.
func Open(ctx context.Context, indexARN string, embed Embedder) (*Index, error) {
	region, err := RegionOf(indexARN)
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	client := s3vectors.NewFromConfig(cfg)
	out, err := client.GetIndex(ctx, &s3vectors.GetIndexInput{IndexArn: awsv2.String(indexARN)})
	if err != nil {
		return nil, fmt.Errorf("describe vector index: %w", err)
	}
	if dim := int(awsv2.ToInt32(out.Index.Dimension)); dim != embed.Dimensions() {
		return nil, fmt.Errorf("vector index has dimension %d but the embedding model produces %d", dim, embed.Dimensions())
	}
	return &Index{client: client, arn: indexARN, embed: embed, metric: out.Index.DistanceMetric}, nil
}

// RegionOf extracts the region from an S3 Vectors index ARN.
func RegionOf(arn string) (string, error) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[0] != "arn" || parts[2] != "s3vectors" || parts[3] == "" {
		return "", fmt.Errorf("invalid S3 Vectors index ARN %q", arn)
	}
	return parts[3], nil
}

// ARN is the index ARN.
func (ix *Index) ARN() string { return ix.arn }

// Info identifies an index for display.
type Info struct {
	ARN    string `json:"arn"`
	Region string `json:"region"`
	Bucket string `json:"bucket"`
	Name   string `json:"name"`
	Metric string `json:"metric"`
}

// Describe parses the index ARN into its parts.
func (ix *Index) Describe() Info {
	info := Info{ARN: ix.arn, Metric: string(ix.metric)}
	info.Region, _ = RegionOf(ix.arn)
	if parts := strings.SplitN(ix.arn, ":", 6); len(parts) == 6 {
		if seg := strings.Split(parts[5], "/"); len(seg) == 4 && seg[0] == "bucket" && seg[2] == "index" {
			info.Bucket, info.Name = seg[1], seg[3]
		}
	}
	return info
}

// Vector is one stored key with its metadata.
type Vector struct {
	Key      string         `json:"key"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Sample returns up to limit vectors and whether more exist.
func (ix *Index) Sample(ctx context.Context, limit int) ([]Vector, bool, error) {
	out := make([]Vector, 0, limit)
	truncated := false
	err := ix.Each(ctx, func(key string, meta map[string]any) bool {
		if len(out) >= limit {
			truncated = true
			return false
		}
		out = append(out, Vector{Key: key, Metadata: meta})
		return true
	})
	return out, truncated, err
}

// Metric is the index's distance metric.
func (ix *Index) Metric() string { return string(ix.metric) }

// Upsert embeds every item and writes it, replacing vectors with the same key.
func (ix *Index) Upsert(ctx context.Context, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	texts := make([]string, len(items))
	for i := range items {
		texts[i] = items[i].Text
	}
	vecs, err := ix.embed.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	if len(vecs) != len(items) {
		return fmt.Errorf("embedder returned %d vectors for %d texts", len(vecs), len(items))
	}
	for start := 0; start < len(items); start += maxBatch {
		end := min(start+maxBatch, len(items))
		batch := make([]types.PutInputVector, 0, end-start)
		for i := start; i < end; i++ {
			v := types.PutInputVector{
				Key:  awsv2.String(items[i].Key),
				Data: &types.VectorDataMemberFloat32{Value: vecs[i]},
			}
			if len(items[i].Metadata) > 0 {
				v.Metadata = document.NewLazyDocument(items[i].Metadata)
			}
			batch = append(batch, v)
		}
		if _, err := ix.client.PutVectors(ctx, &s3vectors.PutVectorsInput{IndexArn: awsv2.String(ix.arn), Vectors: batch}); err != nil {
			return fmt.Errorf("put vectors: %w", err)
		}
	}
	return nil
}

// Query returns the topK vectors closest to text that satisfy filter.
func (ix *Index) Query(ctx context.Context, text string, topK int, filter map[string]any) ([]Match, error) {
	vecs, err := ix.embed.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("embedder returned %d vectors for one text", len(vecs))
	}
	topK = max(1, min(topK, MaxTopK))
	in := &s3vectors.QueryVectorsInput{
		IndexArn:       awsv2.String(ix.arn),
		QueryVector:    &types.VectorDataMemberFloat32{Value: vecs[0]},
		TopK:           awsv2.Int32(int32(topK)),
		ReturnDistance: true,
		ReturnMetadata: true,
	}
	if len(filter) > 0 {
		in.Filter = document.NewLazyDocument(filter)
	}
	out, err := ix.client.QueryVectors(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("query vectors: %w", err)
	}
	matches := make([]Match, 0, len(out.Vectors))
	for _, v := range out.Vectors {
		m := Match{Key: awsv2.ToString(v.Key), Distance: awsv2.ToFloat32(v.Distance)}
		if v.Metadata != nil {
			_ = v.Metadata.UnmarshalSmithyDocument(&m.Metadata)
		}
		matches = append(matches, m)
	}
	return matches, nil
}

// Delete removes the vectors with the given keys; unknown keys are ignored.
func (ix *Index) Delete(ctx context.Context, keys []string) error {
	for start := 0; start < len(keys); start += maxBatch {
		end := min(start+maxBatch, len(keys))
		if _, err := ix.client.DeleteVectors(ctx, &s3vectors.DeleteVectorsInput{IndexArn: awsv2.String(ix.arn), Keys: keys[start:end]}); err != nil {
			return fmt.Errorf("delete vectors: %w", err)
		}
	}
	return nil
}

// Exists reports which of keys are present in the index.
func (ix *Index) Exists(ctx context.Context, keys []string) (map[string]bool, error) {
	found := make(map[string]bool, len(keys))
	for start := 0; start < len(keys); start += maxGetBatch {
		end := min(start+maxGetBatch, len(keys))
		out, err := ix.client.GetVectors(ctx, &s3vectors.GetVectorsInput{IndexArn: awsv2.String(ix.arn), Keys: keys[start:end]})
		if err != nil {
			return nil, fmt.Errorf("get vectors: %w", err)
		}
		for _, v := range out.Vectors {
			found[awsv2.ToString(v.Key)] = true
		}
	}
	return found, nil
}

// Each calls fn with every key in the index and its metadata until fn
// returns false.
func (ix *Index) Each(ctx context.Context, fn func(key string, meta map[string]any) bool) error {
	in := &s3vectors.ListVectorsInput{IndexArn: awsv2.String(ix.arn), MaxResults: awsv2.Int32(maxBatch), ReturnMetadata: true}
	for {
		out, err := ix.client.ListVectors(ctx, in)
		if err != nil {
			return fmt.Errorf("list vectors: %w", err)
		}
		for _, v := range out.Vectors {
			var meta map[string]any
			if v.Metadata != nil {
				_ = v.Metadata.UnmarshalSmithyDocument(&meta)
			}
			if !fn(awsv2.ToString(v.Key), meta) {
				return nil
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return nil
		}
		in.NextToken = out.NextToken
	}
}

// CosineDistance converts a query distance to cosine distance (0 = identical,
// 1 = unrelated) so callers can apply one threshold whatever the metric.
// Vectors are unit length, so euclidean distance d maps to d²/2.
func (ix *Index) CosineDistance(d float32) float32 {
	if ix.metric == types.DistanceMetricEuclidean {
		return d * d / 2
	}
	return d
}

// Eq builds a metadata filter requiring every field to equal its value.
func Eq(fields map[string]any) map[string]any {
	clauses := make([]any, 0, len(fields))
	for k, v := range fields {
		clauses = append(clauses, map[string]any{k: map[string]any{"$eq": v}})
	}
	return And(clauses...)
}

// Ne builds a metadata filter requiring field to differ from value.
func Ne(field string, value any) map[string]any {
	return map[string]any{field: map[string]any{"$ne": value}}
}

// Gte builds a metadata filter requiring field to be at least value.
func Gte(field string, value any) map[string]any {
	return map[string]any{field: map[string]any{"$gte": value}}
}

// And combines filters; nil entries are skipped and a single filter is
// returned as is.
func And(filters ...any) map[string]any {
	clauses := make([]any, 0, len(filters))
	for _, f := range filters {
		if m, ok := f.(map[string]any); ok && len(m) > 0 {
			clauses = append(clauses, m)
		}
	}
	switch len(clauses) {
	case 0:
		return nil
	case 1:
		return clauses[0].(map[string]any)
	}
	return map[string]any{"$and": clauses}
}
