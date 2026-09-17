package blob

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// maxParts is the S3 limit, which B2 and R2 share.
const maxParts = 10000

// Part is one uploaded part: its number and the ETag the bucket returned for
// it, passed back unchanged.
type Part struct {
	Number int
	ETag   string
}

// StartMultipart opens an upload and returns the id the client keeps in
// IndexedDB to resume with.
func (c *Client) StartMultipart(ctx context.Context, key, contentType string) (string, error) {
	if err := validKey(key); err != nil {
		return "", err
	}
	out, err := c.s3.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("blob: start multipart %q: %w", key, err)
	}
	return aws.ToString(out.UploadId), nil
}

// PresignParts returns one URL per part number, in the order asked for.
// Presigning is local, so a batch costs nothing but the signatures.
func (c *Client) PresignParts(ctx context.Context, key, uploadID string, partNumbers []int, ttl time.Duration) ([]string, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	if err := validTTL(ttl); err != nil {
		return nil, err
	}
	urls := make([]string, len(partNumbers))
	for i, n := range partNumbers {
		if n < 1 || n > maxParts {
			return nil, fmt.Errorf("blob: part number %d is outside 1 to %d", n, maxParts)
		}
		req, err := c.presign.PresignUploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(c.bucket),
			Key:        aws.String(key),
			UploadId:   aws.String(uploadID),
			PartNumber: aws.Int32(int32(n)),
		}, s3.WithPresignExpires(ttl))
		if err != nil {
			return nil, fmt.Errorf("blob: presign part %d of %q: %w", n, key, err)
		}
		urls[i] = req.URL
	}
	return urls, nil
}

// ListParts returns every part the bucket already holds, so a client that
// reloaded can upload only what is missing. The listing is paginated at 1000
// parts and an upload runs to 10,000, so every page is read.
func (c *Client) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	pages := s3.NewListPartsPaginator(c.s3, &s3.ListPartsInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	var parts []Part
	for pages.HasMorePages() {
		out, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("blob: list parts of %q: %w", key, err)
		}
		for _, p := range out.Parts {
			parts = append(parts, Part{Number: int(aws.ToInt32(p.PartNumber)), ETag: aws.ToString(p.ETag)})
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

// CompleteMultipart assembles the object. Parts are sorted here because S3
// rejects a list that is not in ascending part order, and the sorted numbers
// must run 1 to len(parts) with no gap: a provider handed a gap either
// assembles a short object or fails in its own way, and the file would be
// marked ready either way.
func (c *Client) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	if err := validKey(key); err != nil {
		return err
	}
	if len(parts) == 0 {
		return fmt.Errorf("blob: complete multipart %q: no parts", key)
	}
	if len(parts) > maxParts {
		return fmt.Errorf("blob: %d parts is above the limit of %d", len(parts), maxParts)
	}
	sorted := make([]Part, len(parts))
	copy(sorted, parts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	completed := make([]types.CompletedPart, len(sorted))
	for i, p := range sorted {
		if p.Number != i+1 {
			return fmt.Errorf("blob: parts must run 1 to %d with no gap, found %d at position %d", len(sorted), p.Number, i+1)
		}
		completed[i] = types.CompletedPart{PartNumber: aws.Int32(int32(p.Number)), ETag: aws.String(p.ETag)}
	}
	if _, err := c.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(c.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		return fmt.Errorf("blob: complete multipart %q: %w", key, err)
	}
	return nil
}

// AbortMultipart throws away an upload and its parts. The sweep for uploads
// abandoned for 48 hours calls this.
func (c *Client) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if err := validKey(key); err != nil {
		return err
	}
	if _, err := c.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	}); err != nil {
		return fmt.Errorf("blob: abort multipart %q: %w", key, err)
	}
	return nil
}
