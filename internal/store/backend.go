package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

var (
	ErrNotFound = errors.New("object not found")
	ErrConflict = errors.New("object was modified concurrently")
)

const (
	defaultRegion = "us-east-1"
	opTimeout     = 20 * time.Second
	listTimeout   = 2 * time.Minute
	maxBodyBytes  = 64 << 20
)

// Backend is the bucket and key prefix holding all service state.
type Backend struct {
	client *s3.Client
	bucket string
	prefix string
	region string
}

// Object describes one stored object. Key is relative to the backend prefix.
type Object struct {
	Key          string
	ETag         string
	Size         int64
	LastModified time.Time
}

// Condition makes a write depend on the object's current state.
type Condition struct {
	IfMatch     string
	IfNoneMatch bool
}

// Open connects to the bucket named by target, which may be an ARN
// (arn:aws:s3:::bucket/prefix), an s3:// URI or bucket/prefix. Credentials
// come from the default AWS chain; the bucket's own region is detected.
func Open(ctx context.Context, target string) (*Backend, error) {
	bucket, prefix := parseTarget(target)
	if bucket == "" {
		return nil, fmt.Errorf("state backend %q: bucket is required", target)
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithDefaultRegion(defaultRegion))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("resolve AWS credentials: %w", err)
	}
	region, err := bucketRegion(ctx, cfg, bucket)
	if err != nil {
		return nil, err
	}
	if prefix != "" {
		prefix += "/"
	}
	return &Backend{
		client: s3.NewFromConfig(cfg, func(o *s3.Options) { o.Region = region }),
		bucket: bucket,
		prefix: prefix,
		region: region,
	}, nil
}

func parseTarget(target string) (bucket, prefix string) {
	t := strings.TrimSpace(target)
	if i := strings.Index(t, ":::"); i >= 0 {
		t = t[i+3:]
	}
	if len(t) >= 5 && strings.EqualFold(t[:5], "s3://") {
		t = t[5:]
	}
	t = strings.Trim(t, "/")
	if i := strings.IndexByte(t, '/'); i >= 0 {
		return t[:i], strings.Trim(t[i+1:], "/")
	}
	return t, ""
}

func bucketRegion(ctx context.Context, cfg awsv2.Config, bucket string) (string, error) {
	out, err := s3.NewFromConfig(cfg).HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awsv2.String(bucket)})
	if err == nil {
		if r := awsv2.ToString(out.BucketRegion); r != "" {
			return r, nil
		}
		return cfg.Region, nil
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		if r := re.Response.Header.Get("x-amz-bucket-region"); r != "" {
			return r, nil
		}
	}
	return "", fmt.Errorf("resolve region of bucket %q: %w", bucket, err)
}

func (b *Backend) String() string { return "s3://" + b.bucket + "/" + b.prefix }

// Region is the region the bucket lives in.
func (b *Backend) Region() string { return b.region }

func (b *Backend) fullKey(key string) *string { return awsv2.String(b.prefix + key) }

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// Get returns the object body and ETag, or ErrNotFound.
func (b *Backend) Get(ctx context.Context, key string) ([]byte, string, error) {
	ctx, cancel := withTimeout(ctx, opTimeout)
	defer cancel()
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{Bucket: awsv2.String(b.bucket), Key: b.fullKey(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, "", ErrNotFound
		}
		return nil, "", fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(out.Body, maxBodyBytes))
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", key, err)
	}
	return body, etag(out.ETag), nil
}

// Put writes body and returns the new ETag. An unmet Condition yields
// ErrConflict; an IfMatch against a missing object yields ErrNotFound.
func (b *Backend) Put(ctx context.Context, key string, body []byte, contentType string, cond Condition) (string, error) {
	ctx, cancel := withTimeout(ctx, opTimeout)
	defer cancel()
	in := &s3.PutObjectInput{Bucket: awsv2.String(b.bucket), Key: b.fullKey(key), Body: bytes.NewReader(body)}
	if contentType != "" {
		in.ContentType = awsv2.String(contentType)
	}
	if cond.IfMatch != "" {
		in.IfMatch = awsv2.String(cond.IfMatch)
	}
	if cond.IfNoneMatch {
		in.IfNoneMatch = awsv2.String("*")
	}
	out, err := b.client.PutObject(ctx, in)
	if err != nil {
		switch {
		case isConflict(err):
			return "", ErrConflict
		case isNotFound(err):
			return "", ErrNotFound
		}
		return "", fmt.Errorf("put %s: %w", key, err)
	}
	return etag(out.ETag), nil
}

// Delete removes an object; a missing object is not an error. With ifMatch
// set, the object is only removed while it still carries that ETag.
func (b *Backend) Delete(ctx context.Context, key, ifMatch string) error {
	ctx, cancel := withTimeout(ctx, opTimeout)
	defer cancel()
	in := &s3.DeleteObjectInput{Bucket: awsv2.String(b.bucket), Key: b.fullKey(key)}
	if ifMatch != "" {
		in.IfMatch = awsv2.String(ifMatch)
	}
	if _, err := b.client.DeleteObject(ctx, in); err != nil {
		switch {
		case isConflict(err):
			return ErrConflict
		case isNotFound(err):
			return nil
		}
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// List returns every object under prefix, following pagination.
func (b *Backend) List(ctx context.Context, prefix string) ([]Object, error) {
	ctx, cancel := withTimeout(ctx, listTimeout)
	defer cancel()
	pager := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: awsv2.String(b.bucket),
		Prefix: b.fullKey(prefix),
	})
	var out []Object
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			obj := Object{Key: strings.TrimPrefix(awsv2.ToString(o.Key), b.prefix), ETag: etag(o.ETag)}
			if o.Size != nil {
				obj.Size = *o.Size
			}
			if o.LastModified != nil {
				obj.LastModified = o.LastModified.UTC()
			}
			out = append(out, obj)
		}
	}
	return out, nil
}

// GetJSON decodes the object at key into a fresh *T.
func GetJSON[T any](ctx context.Context, b *Backend, key string) (*T, string, error) {
	body, tag, err := b.Get(ctx, key)
	if err != nil {
		return nil, "", err
	}
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, "", fmt.Errorf("decode %s: %w", key, err)
	}
	return &v, tag, nil
}

// PutJSON writes v as indented JSON.
func PutJSON(ctx context.Context, b *Backend, key string, v any, cond Condition) (string, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return b.Put(ctx, key, body, "application/json", cond)
}

func etag(p *string) string { return strings.Trim(awsv2.ToString(p), `"`) }

func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var re *awshttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == 404
}

func isConflict(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "PreconditionFailed", "ConditionalRequestConflict":
			return true
		}
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		code := re.HTTPStatusCode()
		return code == 412 || code == 409
	}
	return false
}
