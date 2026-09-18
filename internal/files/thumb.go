package files

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"io"
	"log/slog"

	// The formats the binary can decode. A file of any other type is stored
	// whole and shown without a thumbnail.
	_ "image/gif"
	_ "image/png"

	"golang.org/x/image/draw"

	"github.com/davidtorcivia/theses/internal/blob"
)

// What a thumbnail costs at most. The bytes read bound the download, and the
// pixel count bounds the decode: a small compressed file can declare an image
// of a hundred million pixels, and the header says nothing about how much each
// one costs. A 16 bit per channel PNG with alpha decodes to eight bytes a
// pixel, so the cap is set against that rather than against the four an 8 bit
// image takes: 25 million pixels is 200 MB of decoded image in a process that
// is also serving the app.
const (
	thumbMaxBytes  = 32 << 20
	thumbMaxPixels = 25_000_000
	thumbSide      = 480
	thumbQuality   = 80
)

// thumbnail renders a small JPEG beside the original and returns the image's
// own dimensions, or zeroes when the file is not an image this binary reads.
// Nothing here is worth failing an upload over: a file with no thumbnail still
// downloads, so every refusal is logged and the completion carries on.
func (s *Service) thumbnail(ctx context.Context, bucket *blob.Client, row File) (int64, int64) {
	switch extension(row.Name) {
	case "png", "jpg", "jpeg", "gif":
	default:
		return 0, 0
	}
	if row.Size > thumbMaxBytes {
		return 0, 0
	}
	key := thumbKey(row.ObjectKey)
	if key == "" {
		return 0, 0
	}

	body, err := bucket.Get(ctx, row.ObjectKey)
	if err != nil {
		slog.Warn("could not read an image to make its thumbnail", "file", row.ID, "err", err)
		return 0, 0
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, thumbMaxBytes))
	if err != nil {
		slog.Warn("could not read an image to make its thumbnail", "file", row.ID, "err", err)
		return 0, 0
	}

	// The header is read before the pixels, so an image too large to decode is
	// refused rather than decoded and then refused.
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 ||
		int64(config.Width)*int64(config.Height) > thumbMaxPixels {
		return 0, 0
	}
	source, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return 0, 0
	}

	bounds := source.Bounds()
	width, height := fit(bounds.Dx(), bounds.Dy())
	small := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(small, small.Bounds(), source, bounds, draw.Src, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, small, &jpeg.Options{Quality: thumbQuality}); err != nil {
		slog.Warn("could not encode a thumbnail", "file", row.ID, "err", err)
		return int64(config.Width), int64(config.Height)
	}
	if err := bucket.Put(ctx, key, bytes.NewReader(out.Bytes()), int64(out.Len()), "image/jpeg"); err != nil {
		slog.Warn("could not store a thumbnail", "key", key, "err", err)
	}
	return int64(config.Width), int64(config.Height)
}

// fit is the thumbnail's size: the longer side at thumbSide, the shorter one in
// proportion, and never larger than the original.
func fit(width, height int) (int, int) {
	if width <= thumbSide && height <= thumbSide {
		return width, height
	}
	if width >= height {
		return thumbSide, max(1, height*thumbSide/width)
	}
	return max(1, width*thumbSide/height), thumbSide
}
