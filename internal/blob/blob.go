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
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// PartSize is the multipart part size. B2 allows 5 MiB to 5 GiB parts and
// 10,000 parts, so 64 MiB covers a 640 GiB object.
const PartSize = 64 << 20

// maxPutSize is the S3 limit on a single PutObject; anything larger is a
// multipart upload.
const maxPutSize = 5 << 30

// maxTTL is the SigV4 limit on how long a presigned URL can live. A longer one
// is signed without complaint and fails when the browser uses it.
const maxTTL = 7 * 24 * time.Hour

// requestTimeout bounds how long a response may go silent, not how long a call
// may take. Without it a caller holding context.Background waits forever on an
// endpoint that accepts the connection and then stops talking. It cannot be a
// whole request deadline: S3 answers a large CopyObject or
// CompleteMultipartUpload with a 200 and then trickles whitespace for minutes
// while the storage work finishes, and a deadline would fail those and retry
// the copy.
const requestTimeout = 30 * time.Second

// validKey rejects keys the bucket would not see the way they were signed. A
// browser collapses dot segments before it sends the request, so the signature
// no longer matches, and an endpoint that normalizes the path could resolve
// such a key out of the caller's prefix and into the backups/ prefix the file
// key must never reach.
func validKey(key string) error {
	if key == "" {
		return errors.New("blob: key is empty")
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("blob: key %q starts with a slash", key)
	}
	if strings.Contains(key, "//") {
		return fmt.Errorf("blob: key %q has an empty path segment", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("blob: key %q has a %q segment", key, seg)
		}
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("blob: key %q has a control character", key)
		}
	}
	return nil
}

func validTTL(ttl time.Duration) error {
	if ttl <= 0 || ttl > maxTTL {
		return fmt.Errorf("blob: ttl %s is outside 0 to %s", ttl, maxTTL)
	}
	return nil
}

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

// normalizeEndpoint turns what the owner typed into a base URL the SDK can
// use. Both providers are named by host in their own consoles, so a bare host
// is what most people enter; it gets https, which is the only scheme those
// hosts answer on. An explicit scheme is kept, so a local endpoint can still
// be http. Anything beyond a host is refused rather than quietly signed
// against: the SDK appends the bucket and the key to whatever it is given, so
// a path, a query or credentials in there would sign a URL nobody meant. An
// empty endpoint stays empty, which is how provider s3 asks the SDK to
// resolve AWS itself. What comes back is the scheme and the host the parser
// read, so a scheme typed in capitals reaches the SDK in the form it wants,
// and the refusal quotes what was typed rather than what the scheme was added
// to, so the owner recognizes it.
func normalizeEndpoint(raw string) (string, error) {
	typed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if typed == "" {
		return "", nil
	}
	full := typed
	if !strings.Contains(full, "://") {
		full = "https://" + full
	}
	u, err := url.Parse(full)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !ValidHost(u.Host) ||
		u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("blob: endpoint %q is not an http or https URL", typed)
	}
	return u.Scheme + "://" + u.Host, nil
}

// ValidHost reports whether the Host of a parsed URL is a host and nothing
// else: a name or an address, with a port or an IPv6 literal's brackets
// allowed. url.Parse takes a wildcard such as * or *.example.com without
// complaint, and a wildcard that reaches the settings page becomes a wildcard
// source in the content security policy, so the character set is checked
// rather than assumed. A colon may only sit between a host and a port, never
// at either end, which is what leaves "file:" out; what a port contains
// url.Parse has already checked. The server's CSP builder uses this too, which
// is why it is exported.
func ValidHost(host string) bool {
	if host == "" || !hostEdge(host[0]) || !hostEdge(host[len(host)-1]) {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == ':', r == '[', r == ']':
		default:
			return false
		}
	}
	return true
}

// hostEdge is what a host may begin or end with: a letter or a digit, or the
// brackets around an IPv6 address.
func hostEdge(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '[' || c == ']'
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
		HTTPClient:   awshttp.NewBuildableClient().WithReadTimeout(requestTimeout),
	}
	endpoint, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if endpoint != "" {
		opts.BaseEndpoint = aws.String(endpoint)
	}
	api := s3.New(opts)
	return &Client{s3: api, presign: s3.NewPresignClient(api), bucket: cfg.Bucket}, nil
}

// Bucket is the bucket this client writes to, for a caller deciding whether two
// of its folders land in the same one.
func (c *Client) Bucket() string { return c.bucket }

// PresignPut returns a URL the browser PUTs the whole object to, and the
// headers that were signed into it. The signature pins the content type and
// the length, so an upload of a different size is rejected by the bucket. The
// caller sets Content-Type; Host and Content-Length are on the list because
// they are signed, but a browser fills those in itself and refuses to have
// them set.
func (c *Client) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, map[string]string, error) {
	if err := validKey(key); err != nil {
		return "", nil, err
	}
	if err := validTTL(ttl); err != nil {
		return "", nil, err
	}
	// Size zero would make the SDK drop Content-Length and Content-Type from
	// the signature, leaving a URL that accepts any bytes of any type for the
	// whole TTL. An empty object, if one is ever wanted, is written by the
	// server.
	if size <= 0 || size > maxPutSize {
		return "", nil, fmt.Errorf("blob: size %d is outside 1 to %d", size, int64(maxPutSize))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
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
	if err := validKey(key); err != nil {
		return "", err
	}
	if err := validTTL(ttl); err != nil {
		return "", err
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

// Put writes the whole object from r. size has to be the exact length: SigV4
// signs it, and neither B2 nor R2 takes the chunked signing the SDK would fall
// back to without it. r should be seekable, an *os.File or a *bytes.Reader, so
// the SDK can rewind it to retry.
func (c *Client) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := validKey(key); err != nil {
		return err
	}
	if size < 0 || size > maxPutSize {
		return fmt.Errorf("blob: size %d is outside 0 to %d", size, int64(maxPutSize))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if _, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          r,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	}); err != nil {
		return fmt.Errorf("blob: put %q: %w", key, err)
	}
	return nil
}

// Get opens the object for reading; the caller closes it. ErrNotFound is
// wrapped when the object is not there.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		var nk *types.NoSuchKey
		if errors.As(err, &nk) {
			return nil, fmt.Errorf("blob: get %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("blob: get %q: %w", key, err)
	}
	return out.Body, nil
}

// Object is one listed object.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// List returns every object under prefix in key order, following the bucket's
// pagination. A listing is the only way to read a prefix a key may write to but
// not delete from, which is what the backups prefix is.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	if prefix != "" {
		if err := validKey(prefix); err != nil {
			return nil, err
		}
	}
	var out []Object
	pages := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("blob: list %q: %w", prefix, err)
		}
		for _, o := range page.Contents {
			out = append(out, Object{
				Key:      aws.ToString(o.Key),
				Size:     aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Head returns the stored size and ETag. The ETag is returned as the bucket
// gives it, quotes included, because that is the form CompleteMultipart wants
// back. ErrNotFound is wrapped when the object is not there.
func (c *Client) Head(ctx context.Context, key string) (int64, string, error) {
	if err := validKey(key); err != nil {
		return 0, "", err
	}
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
	if err := validKey(key); err != nil {
		return err
	}
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
	if err := validKey(from); err != nil {
		return err
	}
	if err := validKey(to); err != nil {
		return err
	}
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
func (c *Client) Probe(ctx context.Context) (err error) {
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
	// The object exists from here on, so it is removed whichever step fails.
	// The cleanup drops the caller's cancellation, because the failure being
	// cleaned up is often that same context expiring, and a delete on a dead
	// context never leaves the process. Only the delete on the success path
	// reports its own error, because that is the capability the settings page
	// is testing.
	defer func() {
		if err != nil {
			_ = c.Delete(context.WithoutCancel(ctx), key)
		}
	}()
	if _, _, headErr := c.Head(ctx, key); headErr != nil {
		return fmt.Errorf("blob: probe head %q: %w", key, headErr)
	}
	if delErr := c.Delete(ctx, key); delErr != nil {
		return fmt.Errorf("blob: probe delete %q: %w", key, delErr)
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

// PutCORS sets the rule the browser needs on the bucket: origin may GET, HEAD
// and PUT with any header and read the ETag back. It is the printed rule
// applied for the owner, and like the call underneath it, it replaces whatever
// rule the bucket had rather than adding to it. B2 and R2 both take this call
// and the read back in the AWS form; what they refuse on an object PUT is the
// aws-chunked body, not the checksum header a call like this one carries.
func (c *Client) PutCORS(ctx context.Context, origin string) error {
	if origin == "" {
		return errors.New("blob: origin is empty")
	}
	if _, err := c.s3.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String(c.bucket),
		CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{
			AllowedOrigins: []string{origin},
			AllowedMethods: []string{"GET", "HEAD", "PUT"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  aws.Int32(corsMaxAge),
		}}},
	}); err != nil {
		return fmt.Errorf("blob: put cors on %q: %w", c.bucket, err)
	}
	return nil
}

// CORSAllows reads the bucket's rule back and reports whether origin is in it,
// so the page says what the bucket holds rather than what was sent to it.
func (c *Client) CORSAllows(ctx context.Context, origin string) (bool, error) {
	out, err := c.s3.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(c.bucket)})
	if err != nil {
		return false, fmt.Errorf("blob: get cors on %q: %w", c.bucket, err)
	}
	for _, rule := range out.CORSRules {
		if slices.Contains(rule.AllowedOrigins, origin) {
			return true, nil
		}
	}
	return false, nil
}
