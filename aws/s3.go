// S3 object storage for arbetern. Unlike Cost Explorer (a global service
// signed in us-east-1), S3 is genuinely regional: every object must be
// addressed through a client signed for the bucket's own region, or S3
// answers with a 301 PermanentRedirect. To keep the tool surface
// zero-config — the agent only ever names a bucket and key — the bucket's
// region is auto-detected once (via HeadBucket) and cached, and one S3
// client is built per region on demand. Credentials come from the same
// default SDK chain as the rest of the aws package (env / profile / IRSA).
package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// maxS3InlineBytes caps how much of an object S3GetObject returns inline.
// Objects larger than this are truncated (with Truncated=true) so a stray
// read of a multi-megabyte file can't blow up the LLM context window. 1 MiB
// comfortably holds a day's worth of CSV rows.
const maxS3InlineBytes = 1 << 20

// maxS3ListKeys caps a single ListObjects page. S3 itself allows up to 1000
// keys per page; we never paginate beyond the first page to keep tool calls
// cheap and bounded.
const maxS3ListKeys = 1000

// --------------------------------------------------------------------------
// Public result types
// --------------------------------------------------------------------------

// S3PutResult summarises a completed PutObject.
type S3PutResult struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	Region    string `json:"region"`
	Size      int    `json:"size"`
	ETag      string `json:"etag,omitempty"`
	VersionID string `json:"version_id,omitempty"`
}

// S3GetResult holds an object's (possibly truncated) body and metadata.
type S3GetResult struct {
	Bucket       string `json:"bucket"`
	Key          string `json:"key"`
	Region       string `json:"region"`
	Size         int64  `json:"size"`
	ContentType  string `json:"content_type,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Body         string `json:"body"`
	Truncated    bool   `json:"truncated"`
}

// S3Object is one row of a listing.
type S3Object struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	LastModified string `json:"last_modified,omitempty"`
	ETag         string `json:"etag,omitempty"`
}

// S3ListResult is a single (capped) page of a prefix listing.
type S3ListResult struct {
	Bucket    string     `json:"bucket"`
	Region    string     `json:"region"`
	Prefix    string     `json:"prefix,omitempty"`
	Objects   []S3Object `json:"objects"`
	Truncated bool       `json:"truncated"`
}

// --------------------------------------------------------------------------
// Operations
// --------------------------------------------------------------------------

// S3PutObject writes body to s3://bucket/key. contentType is optional; when
// empty it is inferred from the key's extension (.csv/.json/.txt) and
// otherwise left for S3 to default. bucket may be a bare name, an
// "s3://bucket/key" URI, or an "arn:aws:s3:::bucket/key" ARN — any embedded
// key path is merged with key.
func (c *Client) S3PutObject(ctx context.Context, bucket, key string, body []byte, contentType string) (*S3PutResult, error) {
	bucket, key = normalizeS3Target(bucket, key)
	if bucket == "" {
		return nil, fmt.Errorf("bucket is required")
	}
	if key == "" {
		return nil, fmt.Errorf("key is required")
	}
	cl, region, err := c.s3ClientForBucket(ctx, bucket)
	if err != nil {
		return nil, err
	}
	in := &s3.PutObjectInput{
		Bucket: awsv2.String(bucket),
		Key:    awsv2.String(key),
		Body:   bytes.NewReader(body),
	}
	if ct := strings.TrimSpace(contentType); ct != "" {
		in.ContentType = awsv2.String(ct)
	} else if ct := contentTypeForKey(key); ct != "" {
		in.ContentType = awsv2.String(ct)
	}
	out, err := cl.PutObject(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("PutObject s3://%s/%s: %w", bucket, key, err)
	}
	return &S3PutResult{
		Bucket:    bucket,
		Key:       key,
		Region:    region,
		Size:      len(body),
		ETag:      strings.Trim(awsv2.ToString(out.ETag), `"`),
		VersionID: awsv2.ToString(out.VersionId),
	}, nil
}

// S3GetObject reads s3://bucket/key, returning the body as a string. Bodies
// larger than maxS3InlineBytes are truncated and Truncated is set.
func (c *Client) S3GetObject(ctx context.Context, bucket, key string) (*S3GetResult, error) {
	bucket, key = normalizeS3Target(bucket, key)
	if bucket == "" {
		return nil, fmt.Errorf("bucket is required")
	}
	if key == "" {
		return nil, fmt.Errorf("key is required")
	}
	cl, region, err := c.s3ClientForBucket(ctx, bucket)
	if err != nil {
		return nil, err
	}
	out, err := cl.GetObject(ctx, &s3.GetObjectInput{
		Bucket: awsv2.String(bucket),
		Key:    awsv2.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("GetObject s3://%s/%s: %w", bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()

	// Read one byte past the cap so we can tell "exactly at the cap" from
	// "larger than the cap".
	data, err := io.ReadAll(io.LimitReader(out.Body, maxS3InlineBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read s3://%s/%s: %w", bucket, key, err)
	}
	res := &S3GetResult{Bucket: bucket, Key: key, Region: region}
	if len(data) > maxS3InlineBytes {
		data = data[:maxS3InlineBytes]
		res.Truncated = true
	}
	res.Body = string(data)
	if out.ContentLength != nil {
		res.Size = *out.ContentLength
	}
	if out.ContentType != nil {
		res.ContentType = *out.ContentType
	}
	if out.LastModified != nil {
		res.LastModified = out.LastModified.UTC().Format(time.RFC3339)
	}
	return res, nil
}

// S3ListObjects lists up to maxKeys objects under s3://bucket/prefix (one
// page, capped at maxS3ListKeys). prefix is optional. Results are sorted by
// key so date-stamped objects (e.g. 2026-06-11.csv) come back in order.
func (c *Client) S3ListObjects(ctx context.Context, bucket, prefix string, maxKeys int) (*S3ListResult, error) {
	bucket, prefix = normalizeS3Target(bucket, prefix)
	if bucket == "" {
		return nil, fmt.Errorf("bucket is required")
	}
	cl, region, err := c.s3ClientForBucket(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if maxKeys <= 0 || maxKeys > maxS3ListKeys {
		maxKeys = maxS3ListKeys
	}
	in := &s3.ListObjectsV2Input{
		Bucket:  awsv2.String(bucket),
		MaxKeys: awsv2.Int32(int32(maxKeys)),
	}
	if p := strings.TrimSpace(prefix); p != "" {
		in.Prefix = awsv2.String(p)
	}
	out, err := cl.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("ListObjectsV2 s3://%s: %w", bucket, err)
	}
	res := &S3ListResult{Bucket: bucket, Region: region, Prefix: prefix}
	if out.IsTruncated != nil {
		res.Truncated = *out.IsTruncated
	}
	for _, o := range out.Contents {
		obj := S3Object{
			Key:  awsv2.ToString(o.Key),
			ETag: strings.Trim(awsv2.ToString(o.ETag), `"`),
		}
		if o.Size != nil {
			obj.Size = *o.Size
		}
		if o.LastModified != nil {
			obj.LastModified = o.LastModified.UTC().Format(time.RFC3339)
		}
		res.Objects = append(res.Objects, obj)
	}
	sort.Slice(res.Objects, func(i, j int) bool { return res.Objects[i].Key < res.Objects[j].Key })
	return res, nil
}

// --------------------------------------------------------------------------
// Region-aware client plumbing
// --------------------------------------------------------------------------

// s3ClientForBucket returns an S3 client signed for bucket's region (and the
// region itself), detecting and caching the region on first use.
func (c *Client) s3ClientForBucket(ctx context.Context, bucket string) (*s3.Client, string, error) {
	c.s3mu.Lock()
	defer c.s3mu.Unlock()
	region, err := c.bucketRegionLocked(ctx, bucket)
	if err != nil {
		return nil, "", err
	}
	return c.s3ForRegionLocked(region), region, nil
}

// bucketRegionLocked resolves (and caches) bucket's region. It probes with a
// client in the default signing region: a same-region bucket answers 200 and
// echoes BucketRegion, a cross-region bucket answers a redirect/4xx whose
// x-amz-bucket-region header still names the home region. Caller must hold
// s3mu.
func (c *Client) bucketRegionLocked(ctx context.Context, bucket string) (string, error) {
	if r, ok := c.bucketRegions[bucket]; ok {
		return r, nil
	}
	probe := c.s3ForRegionLocked(c.region)
	out, err := probe.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awsv2.String(bucket)})
	if err == nil {
		region := c.region
		if out.BucketRegion != nil && *out.BucketRegion != "" {
			region = *out.BucketRegion
		}
		c.bucketRegions[bucket] = region
		return region, nil
	}
	// A redirect or access-denied response still carries the bucket's home
	// region in the x-amz-bucket-region header.
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		if r := re.Response.Header.Get("x-amz-bucket-region"); r != "" {
			c.bucketRegions[bucket] = r
			return r, nil
		}
	}
	return "", fmt.Errorf("resolve region for bucket %q: %w", bucket, err)
}

// s3ForRegionLocked returns a cached S3 client for region, creating it on
// first use. Caller must hold s3mu.
func (c *Client) s3ForRegionLocked(region string) *s3.Client {
	if region == "" {
		region = c.region
	}
	if cl, ok := c.s3Clients[region]; ok {
		return cl
	}
	cl := s3.NewFromConfig(c.cfg, func(o *s3.Options) { o.Region = region })
	c.s3Clients[region] = cl
	return cl
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// normalizeS3Target accepts a bucket that may be a bare name, an
// "s3://bucket/key" URI, or an "arn:aws:s3:::bucket/key" ARN, plus an
// optional separate key. It returns the resolved (bucket, key). When the
// bucket carries an embedded key path it becomes the key prefix and the
// separate key (if any) is appended beneath it. This lets a caller paste an
// ARN straight into the bucket field and still get a clean split.
func normalizeS3Target(bucket, key string) (string, string) {
	b := strings.TrimSpace(bucket)
	k := strings.TrimSpace(key)
	// Strip an ARN prefix (arn:aws:s3:::bucket/key) down to bucket/key.
	if i := strings.Index(b, ":::"); i >= 0 {
		b = b[i+3:]
	}
	// Strip an s3:// scheme.
	if len(b) >= 5 && strings.EqualFold(b[:5], "s3://") {
		b = b[5:]
	}
	b = strings.TrimPrefix(b, "/")
	// Split an embedded key path out of the bucket field.
	if i := strings.IndexByte(b, '/'); i >= 0 {
		embedded := strings.Trim(b[i+1:], "/")
		b = b[:i]
		switch {
		case k == "":
			k = embedded
		case embedded != "":
			k = embedded + "/" + strings.TrimPrefix(k, "/")
		}
	}
	return b, k
}

// contentTypeForKey returns a sensible Content-Type for a key based on its
// extension, or "" to let S3 default. Kept tiny on purpose — only the
// formats arbetern actually writes.
func contentTypeForKey(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	case ".txt", ".log":
		return "text/plain"
	case ".md":
		return "text/markdown"
	}
	return ""
}

// humanSize renders a byte count compactly (e.g. "1.2 KB") for listings.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// --------------------------------------------------------------------------
// Slack formatting
// --------------------------------------------------------------------------

// S3URI returns the canonical s3://bucket/key reference for an object.
func S3URI(bucket, key string) string {
	return fmt.Sprintf("s3://%s/%s", bucket, key)
}

// S3ConsoleURL builds a deep link to an object in the AWS S3 web console. A
// signed-in operator can click it to open the object's detail page. This is
// NOT a public/REST object URL (https://bucket.s3.region.amazonaws.com/key),
// which returns AccessDenied in a browser — it is the console UI link.
func S3ConsoleURL(bucket, key, region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf(
		"https://%s.console.aws.amazon.com/s3/object/%s?region=%s&bucketType=general&prefix=%s",
		url.QueryEscape(region), url.PathEscape(bucket), url.QueryEscape(region), url.QueryEscape(key),
	)
}

// FormatS3Put renders a PutObject confirmation for Slack.
func FormatS3Put(r *S3PutResult) string {
	if r == nil {
		return "No S3 write result."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Wrote %s to `s3://%s/%s`* (region %s)", humanSize(int64(r.Size)), r.Bucket, r.Key, r.Region)
	if r.ETag != "" {
		fmt.Fprintf(&sb, "\nETag: `%s`", r.ETag)
	}
	if r.VersionID != "" {
		fmt.Fprintf(&sb, "\nVersion: `%s`", r.VersionID)
	}
	return sb.String()
}

// FormatS3Get renders an object body (in a code block) plus metadata.
func FormatS3Get(r *S3GetResult) string {
	if r == nil {
		return "No object returned."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*`s3://%s/%s`* — %s, region %s", r.Bucket, r.Key, humanSize(r.Size), r.Region)
	if r.LastModified != "" {
		fmt.Fprintf(&sb, ", modified %s", r.LastModified)
	}
	sb.WriteString("\n```\n")
	sb.WriteString(r.Body)
	if !strings.HasSuffix(r.Body, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("```")
	if r.Truncated {
		fmt.Fprintf(&sb, "\n_(truncated at %s — object is larger; fetch a narrower key or process in chunks)_", humanSize(maxS3InlineBytes))
	}
	return sb.String()
}

// FormatS3List renders a prefix listing as a compact table.
func FormatS3List(r *S3ListResult) string {
	if r == nil {
		return "No objects found."
	}
	if len(r.Objects) == 0 {
		return fmt.Sprintf("No objects under `s3://%s/%s`.", r.Bucket, r.Prefix)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Objects under `s3://%s/%s`* (%d", r.Bucket, r.Prefix, len(r.Objects))
	if r.Truncated {
		sb.WriteString("+, truncated")
	}
	sb.WriteString(")\n```\n")
	for _, o := range r.Objects {
		lm := o.LastModified
		if lm == "" {
			lm = "-"
		}
		fmt.Fprintf(&sb, "%-20s  %10s  %s\n", lm, humanSize(o.Size), o.Key)
	}
	sb.WriteString("```")
	return sb.String()
}
