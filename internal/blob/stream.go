package blob

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// PutStream writes an object from a reader that cannot be rewound, which is
// what a body arriving from somewhere else is. Put signs the payload, so it
// asks the reader to seek back to the start once it has hashed it; a network
// body has no start to go back to. This one sends UNSIGNED-PAYLOAD instead,
// which is what the signature on a presigned PUT already leaves out and what
// B2 and R2 both take, so the bytes go straight through without ever being
// held in memory or on disk.
//
// size has to be the exact length. It is signed, so the bucket refuses a body
// that is not that long, and the caller checks the stored object afterwards
// besides.
func (c *Client) PutStream(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := validKey(key); err != nil {
		return err
	}
	if size <= 0 || size > maxPutSize {
		return fmt.Errorf("blob: size %d is outside 1 to %d", size, int64(maxPutSize))
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
	}, s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)); err != nil {
		return fmt.Errorf("blob: put %q: %w", key, err)
	}
	return nil
}
