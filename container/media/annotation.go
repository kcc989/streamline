package media

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
)

// DecodeAnnotationPNG validates a complete bounded annotation snapshot. Both
// live and timestamped recordings use full-canvas RGBA PNGs from the caller.
func DecodeAnnotationPNG(data []byte, width, height int) (image.Image, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != width || cfg.Height != height {
		return nil, fmt.Errorf("invalid annotation dimensions")
	}
	reader := bytes.NewReader(data)
	img, err := png.Decode(reader)
	if err != nil || reader.Len() != 0 {
		return nil, fmt.Errorf("invalid annotation PNG")
	}
	return img, nil
}
