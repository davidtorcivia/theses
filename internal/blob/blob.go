// Package blob is the object storage client: presigned uploads and downloads
// against Backblaze B2, Cloudflare R2 or any other S3 endpoint. Keys are built
// by the caller; nothing here knows what a proposition is.
package blob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// PartSize is the multipart part size. B2 allows 5 MiB to 5 GiB parts and
// 10,000 parts, so 64 MiB covers a 640 GiB object.
const PartSize = 64 << 20

// Config is one bucket. Provider is b2, r2 or s3.
type Config struct {
	Provider      string
	Endpoint      string
	Region        string
	Bucket        string
	AccessKey     string
	SecretKey     string
	PublicBaseURL string
}

// Defaults fills endpoint and region from the provider and a hint: the B2
// region such as us-west-004, or the R2 account id. The owner still supplies
// bucket and keys. For provider s3 the endpoint is left empty so the SDK
// resolves the AWS endpoint for the hint region.
func Defaults(provider, hint string) Config {
	c := Config{Provider: provider}
	switch provider {
	case "b2":
		c.Region = hint
		c.Endpoint = "https://s3." + hint + ".backblazeb2.com"
	case "r2":
		c.Region = "auto"
		c.Endpoint = "https://" + hint + ".r2.cloudflarestorage.com"
	case "s3":
		c.Region = hint
	}
	return c
}

// Client talks to one bucket.
type Client struct {
	s3      *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// New validates cfg and builds the client. It makes no network call.
func New(cfg Config) (*Client, error) {
	switch cfg.Provider {
	case "b2", "r2", "s3":
	default:
		return nil, fmt.Errorf("blob: unknown provider %q", cfg.Provider)
	}
	if cfg.Bucket == "" {
		return nil, errors.New("blob: bucket is empty")
	}
	if cfg.Region == "" {
		return nil, errors.New("blob: region is empty")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("blob: access key or secret key is empty")
	}
	opts := s3.Options{
		Region: cfg.Region,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: cfg.AccessKey, SecretAccessKey: cfg.SecretKey, Source: "theses settings"}, nil
		}),
		// B2 and R2 reject the aws-chunked bodies and x-amz-checksum headers
		// the SDK adds by default, and a browser cannot compute them for a
		// presigned PUT either.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		// B2, R2 and gofakes3 all serve path style; only AWS needs virtual
		// hosted addressing and it accepts path style too.
		UsePathStyle: true,
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("blob: endpoint %q: %w", cfg.Endpoint, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("blob: endpoint %q is not an http or https URL", cfg.Endpoint)
		}
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	api := s3.New(opts)
	return &Client{s3: api, presign: s3.NewPresignClient(api), bucket: cfg.Bucket}, nil
}

// PresignPut returns a URL the browser PUTs the whole object to, and the
// headers that were signed into it. The signature pins the content type and
// the length, so an upload of a different size is rejected by the bucket. The
// caller sets Content-Type; Host and Content-Length are on the list because
// they are signed, but a browser fills those in itself and refuses to have
// them set.
func (c *Client) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, map[string]string, error) {
	if key == "" {
		return "", nil, errors.New("blob: key is empty")
	}
	if size <= 0 {
		return "", nil, fmt.Errorf("blob: size %d is not positive", size)
	}
	req, err := c.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", nil, fmt.Errorf("blob: presign put %q: %w", key, err)
	}
	return req.URL, flatten(req.SignedHeader), nil
}

// PresignGet returns a download URL. filename, when not empty, becomes a
// Content-Disposition attachment through response-content-disposition, so the
// browser saves the object under its original name rather than its key.
func (c *Client) PresignGet(ctx context.Context, key, filename string, ttl time.Duration) (string, error) {
	if key == "" {
		return "", errors.New("blob: key is empty")
	}
	in := &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)}
	if filename != "" {
		d := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
		if d == "" {
			return "", fmt.Errorf("blob: filename %q cannot be put in a Content-Disposition header", filename)
		}
		in.ResponseContentDisposition = aws.String(d)
	}
	req, err := c.presign.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("blob: presign get %q: %w", key, err)
	}
	return req.URL, nil
}

// Head returns the stored size and ETag. The ETag is returned as the bucket
// gives it, quotes included, because that is the form CompleteMultipart wants
// back. ErrNotFound is wrapped when the object is not there.
func (c *Client) Head(ctx context.Context, key string) (int64, string, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		var nf *types.NotFound
		if errors.As(err, &nf) {
			return 0, "", fmt.Errorf("blob: head %q: %w", key, ErrNotFound)
		}
		return 0, "", fmt.Errorf("blob: head %q: %w", key, err)
	}
	return aws.ToInt64(out.ContentLength), aws.ToString(out.ETag), nil
}

// ErrNotFound lets callers check for a missing object without importing the
// AWS error types.
var ErrNotFound = errors.New("object not found")

// Delete removes the object. Deleting something that is not there succeeds.
func (c *Client) Delete(ctx context.Context, key string) error {
	if _, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)}); err != nil {
		return fmt.Errorf("blob: delete %q: %w", key, err)
	}
	return nil
}

// Copy duplicates an object inside the bucket, for a new version or a rename.
//
// ponytail: same bucket and 5 GiB only, which is what versions need; moving a
// folder to another bucket adds a source bucket parameter, and objects over
// 5 GiB need UploadPartCopy instead.
func (c *Client) Copy(ctx context.Context, from, to string) error {
	src := (&url.URL{Path: c.bucket + "/" + from}).EscapedPath()
	if _, err := c.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(c.bucket),
		Key:        aws.String(to),
		CopySource: aws.String(src),
	}); err != nil {
		return fmt.Errorf("blob: copy %q to %q: %w", from, to, err)
	}
	return nil
}

// Probe writes, heads and deletes a small object so the settings page can say
// which of the three failed. It is the "Test connection" button.
func (c *Client) Probe(ctx context.Context) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("blob: probe: %w", err)
	}
	key := "probe/" + hex.EncodeToString(b[:])
	const body = "theses probe\n"
	if _, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          strings.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String("text/plain"),
	}); err != nil {
		return fmt.Errorf("blob: probe put %q: %w", key, err)
	}
	if _, _, err := c.Head(ctx, key); err != nil {
		return fmt.Errorf("blob: probe head %q: %w", key, err)
	}
	if err := c.Delete(ctx, key); err != nil {
		return fmt.Errorf("blob: probe delete %q: %w", key, err)
	}
	return nil
}

// CORSRuleB2 is the rule the owner pastes into `b2 bucket update --cors-rules`
// or the B2 API. B2 keeps its native rule shape even for the S3 endpoint, with
// its own operation names, so it cannot share a document with R2.
func CORSRuleB2(origin string) string {
	return marshal([]any{struct {
		Name          string   `json:"corsRuleName"`
		Origins       []string `json:"allowedOrigins"`
		Operations    []string `json:"allowedOperations"`
		Headers       []string `json:"allowedHeaders"`
		ExposeHeaders []string `json:"exposeHeaders"`
		MaxAgeSeconds int      `json:"maxAgeSeconds"`
	}{
		Name:          "theses",
		Origins:       []string{origin},
		Operations:    []string{"s3_get", "s3_head", "s3_put"},
		Headers:       []string{"*"},
		ExposeHeaders: []string{"ETag"},
		MaxAgeSeconds: corsMaxAge,
	}})
}

// CORSRuleR2 is the rule the owner pastes into the R2 dashboard, which takes
// the S3 CORSRule shape. Any other S3 provider takes this shape too.
func CORSRuleR2(origin string) string {
	return marshal([]any{struct {
		Origins       []string `json:"AllowedOrigins"`
		Methods       []string `json:"AllowedMethods"`
		Headers       []string `json:"AllowedHeaders"`
		ExposeHeaders []string `json:"ExposeHeaders"`
		MaxAgeSeconds int      `json:"MaxAgeSeconds"`
	}{
		Origins:       []string{origin},
		Methods:       []string{"GET", "HEAD", "PUT"},
		Headers:       []string{"*"},
		ExposeHeaders: []string{"ETag"},
		MaxAgeSeconds: corsMaxAge,
	}})
}

const corsMaxAge = 3600

func marshal(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// The values are strings and ints from this package; nothing can fail.
		panic(err)
	}
	return string(b)
}

func flatten(h http.Header) map[string]string {
	m := make(map[string]string, len(h))
	for k, v := range h {
		m[k] = strings.Join(v, ", ")
	}
	return m
}
