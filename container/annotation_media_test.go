package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/cloudflare/streamline/container/ffmpeg"
	"github.com/cloudflare/streamline/container/media"
	"github.com/stretchr/testify/require"
)

// Exercise the upstream PNG annotation graph with synthetic cursor/click
// snapshots. No cursor-specific engine or pipeline operation is required.
func TestExistingAnnotationGraphRendersTimedCursorAndClick(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	webm, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i",
		"color=black:s=64x48:r=10:d=1", "-c:v", "libvpx", "-f", "webm", "pipe:1").Output()
	require.NoError(t, err)
	var snapshots bytes.Buffer
	for tick := 0; tick < media.AnnotationOverlayFPS; tick++ {
		m := image.NewNRGBA(image.Rect(0, 0, 64, 48))
		if tick < 4 {
			x := 8
			if tick >= 2 {
				x = 32
			}
			draw.Draw(m, image.Rect(x, 8, x+8, 16), image.NewUniform(color.White), image.Point{}, draw.Src)
			if tick >= 2 {
				// An opaque click marker can be drawn by the caller in the same PNG.
				draw.Draw(m, image.Rect(30, 20, 42, 26), image.NewUniform(color.RGBA{R: 255, A: 255}), image.Point{}, draw.Src)
			}
		}
		var snapshot bytes.Buffer
		require.NoError(t, png.Encode(&snapshot, m))
		_, _, err := validateAnnotationPNG(snapshot.Bytes())
		require.NoError(t, err)
		snapshots.Write(snapshot.Bytes())
	}
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	args, err := ffmpeg.BuildArgs(media.SessionConfig{SourceType: media.SourceWebcam,
		OutputMode: media.OutputPreview, AnnotationOverlay: true,
		Encode: media.EncodeConfig{Resolution: "64x48", FPS: 10, GOP: 10, Preset: "ultrafast"}})
	require.NoError(t, err)
	args = append(args[:len(args)-1], "-t", "1", args[len(args)-1])
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin = bytes.NewReader(webm)
	cmd.ExtraFiles = []*os.File{reader}
	done := make(chan error, 1)
	go func() { _, err := writer.Write(snapshots.Bytes()); writer.Close(); done <- err }()
	output, err := cmd.Output()
	require.NoError(t, err)
	require.NoError(t, <-done)
	decode := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-i", "pipe:0", "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
	decode.Stdin = bytes.NewReader(output)
	raw, err := decode.Output()
	require.NoError(t, err)
	require.Len(t, raw, 64*48*3*10)
	pixel := func(frame, x, y int) []byte { offset := (frame*64*48 + y*64 + x) * 3; return raw[offset : offset+3] }
	require.Greater(t, pixel(0, 12, 12)[0], byte(230))
	require.Less(t, pixel(0, 36, 12)[0], byte(20))
	require.Greater(t, pixel(5, 36, 12)[0], byte(230))
	require.Less(t, pixel(5, 12, 12)[0], byte(20))
	require.Greater(t, pixel(5, 36, 23)[0], byte(200))
	require.Less(t, pixel(9, 36, 12)[0], byte(20))
}
