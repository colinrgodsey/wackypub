package agent

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"

	xdraw "golang.org/x/image/draw"
)

const (
	DefaultJPEGQuality   = 85
	MaxMediaPayloadBytes = 10 * 1024 * 1024 // 10MB ceiling (D47, D112)
)

// NormalizeAndResizeImage reads an image from r, detects format, flattens any transparency onto white,
// resizes so the longest side does not exceed maxDimension (downscale only), and re-encodes as JPEG.
func NormalizeAndResizeImage(r io.Reader, maxDimension int) ([]byte, string, error) {
	if maxDimension <= 0 {
		return nil, "", fmt.Errorf("maxImageDimension must be greater than 0")
	}

	srcData, err := io.ReadAll(r)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read image data: %w", err)
	}

	if len(srcData) == 0 {
		return nil, "", fmt.Errorf("empty image data")
	}

	img, _, err := image.Decode(bytes.NewReader(srcData))
	if err != nil {
		return nil, "", fmt.Errorf("unsupported or invalid image format: %w", err)
	}

	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	if width <= 0 || height <= 0 {
		return nil, "", fmt.Errorf("invalid image dimensions: %dx%d", width, height)
	}

	targetWidth := width
	targetHeight := height

	maxSide := width
	if height > maxSide {
		maxSide = height
	}

	if maxSide > maxDimension {
		if width >= height {
			targetWidth = maxDimension
			targetHeight = (height * maxDimension) / width
		} else {
			targetHeight = maxDimension
			targetWidth = (width * maxDimension) / height
		}
		if targetWidth < 1 {
			targetWidth = 1
		}
		if targetHeight < 1 {
			targetHeight = 1
		}
	}

	dst := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))

	// Fill background with solid white to flatten any transparency
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: white}, image.Point{}, draw.Src)

	if targetWidth != width || targetHeight != height {
		// Rescaling required
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
	} else {
		// Exact size, composite over white
		draw.Draw(dst, dst.Bounds(), img, bounds.Min, draw.Over)
	}

	var buf bytes.Buffer
	err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: DefaultJPEGQuality})
	if err != nil {
		return nil, "", fmt.Errorf("failed to encode JPEG: %w", err)
	}

	return buf.Bytes(), "image/jpeg", nil
}
