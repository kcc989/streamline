// recording-demo renders entirely synthetic media without a Worker deployment.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"

	"github.com/cloudflare/streamline/container/recording"
)

func main() {
	output := flag.String("output", "recording.mp4", "new output file (must not exist)")
	flag.Parse()
	if err := run(*output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output string) error {
	cfg := recording.Config{Width: 320, Height: 240, FPS: 10, Masks: []recording.Mask{
		{StartMS: 0, EndMS: 1000, X: 50, Y: 70, Width: 80, Height: 40},
	}}
	r, err := recording.New(cfg)
	if err != nil {
		return err
	}
	defer r.Cancel()
	sink, err := recording.NewLocalSink()
	if err != nil {
		return err
	}
	defer sink.Close()
	for index, timestamp := range []int64{0, 750, 1500} {
		canvas := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
		background := []color.RGBA{{R: 180, G: 80, B: 50, A: 255}, {R: 40, G: 100, B: 180, A: 255}, {R: 50, G: 160, B: 90, A: 255}}[index]
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
		var b bytes.Buffer
		if err := jpeg.Encode(&b, canvas, &jpeg.Options{Quality: 95}); err != nil {
			return err
		}
		if err := r.AddFrame(index, timestamp, b.Bytes()); err != nil {
			return err
		}
	}
	for index, timestamp := range []int64{0, 500, 1000, 1500} {
		canvas := image.NewNRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
		if index < 3 {
			x := 80 + index*60
			draw.Draw(canvas, image.Rect(x, 90, x+8, 110), image.NewUniform(color.White), image.Point{}, draw.Src)
			if index == 1 {
				draw.Draw(canvas, image.Rect(x-10, 115, x+18, 123), image.NewUniform(color.RGBA{R: 255, G: 210, A: 255}), image.Point{}, draw.Src)
			}
		}
		var b bytes.Buffer
		if err := png.Encode(&b, canvas); err != nil {
			return err
		}
		if err := r.AddAnnotation(index, timestamp, b.Bytes()); err != nil {
			return err
		}
	}
	pcm := make([]byte, recording.SampleRate*2)
	for i := 0; i < recording.SampleRate; i++ {
		sample := int16(7000 * math.Sin(2*math.Pi*440*float64(i)/recording.SampleRate))
		binary.LittleEndian.PutUint16(pcm[i*2:i*2+2], uint16(sample))
	}
	if err := r.AddNarration(0, 500, 1000, pcm); err != nil {
		return err
	}
	a, err := r.Finalize(context.Background(), 2000, sink)
	if err != nil {
		return err
	}
	input, err := sink.Open(a.ID)
	if err != nil {
		return err
	}
	defer input.Close()
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, input)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(output)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(output)
		return closeErr
	}
	fmt.Printf("Synthetic MP4: %s (%d bytes, %d ms)\n", output, a.Bytes, a.DurationMS)
	return nil
}
