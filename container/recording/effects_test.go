package recording

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func annotationPNG(t *testing.T, rect image.Rectangle) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	draw.Draw(m, rect, image.NewUniform(color.White), image.Point{}, draw.Src)
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, m))
	return b.Bytes()
}

func TestMasksApplyOnEveryHeldFrameAndAfterAnnotation(t *testing.T) {
	r, s := newTestRecorder(t)
	r.cfg.Masks = []Mask{{StartMS: 0, EndMS: 400, X: 20, Y: 20, Width: 4, Height: 4},
		{StartMS: 600, EndMS: 800, X: 20, Y: 20, Width: 4, Height: 4}}
	require.NoError(t, r.AddFrame(0, 0, jpegFrame(t, color.RGBA{R: 255, A: 255})))
	overlay := annotationPNG(t, image.Rect(18, 18, 28, 28))
	require.NoError(t, r.AddAnnotation(0, 0, overlay))
	require.NoError(t, r.AddAnnotation(0, 0, overlay))
	require.ErrorIs(t, r.AddAnnotation(0, 1, overlay), ErrConflict)
	require.ErrorIs(t, r.AddAnnotation(2, 500, overlay), ErrConflict)
	require.ErrorIs(t, r.AddAnnotation(1, 500, []byte("PNG missing")), ErrInvalid)
	require.NoError(t, r.AddAnnotation(1, 800, annotationPNG(t, image.Rectangle{})))
	_, err := r.Finalize(context.Background(), 1000, s)
	require.NoError(t, err)
	f, err := s.Open(r.ID())
	require.NoError(t, err)
	defer f.Close()
	raw, err := exec.Command("ffmpeg", "-v", "error", "-i", f.Name(), "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1").Output()
	require.NoError(t, err)
	require.Len(t, raw, 10*64*48*3)
	for tick := 0; tick < 10; tick++ {
		offset := (tick*64*48 + 22*64 + 22) * 3
		pixel := raw[offset : offset+3]
		if tick < 4 || (tick >= 6 && tick < 8) {
			// Check every pixel in the requested private rectangle.
			for y := 20; y < 24; y++ {
				for x := 20; x < 24; x++ {
					off := (tick*64*48 + y*64 + x) * 3
					for _, channel := range raw[off : off+3] {
						require.Less(t, channel, byte(8))
					}
				}
			}
		} else if tick < 8 {
			for _, channel := range pixel {
				require.Greater(t, channel, byte(230))
			}
		} else {
			require.Greater(t, pixel[0], byte(240))
			require.Less(t, pixel[1], byte(10))
		}
	}
}

func TestInvalidMasksAndImmutablePlan(t *testing.T) {
	for _, mask := range []Mask{{StartMS: 10, EndMS: 10, Width: 1, Height: 1},
		{StartMS: 0, EndMS: 10, X: 63, Width: 2, Height: 1}, {StartMS: 0, EndMS: 10, Width: 1 << 62, Height: 1},
		{StartMS: -1, EndMS: 10, Width: 1, Height: 1}} {
		_, err := New(Config{Width: 64, Height: 48, FPS: 10, Masks: []Mask{mask}})
		require.ErrorIs(t, err, ErrInvalid)
	}
	plan := []Mask{{StartMS: 0, EndMS: 1000, Width: 64, Height: 48}}
	r, err := New(Config{Width: 64, Height: 48, FPS: 10, Masks: plan})
	require.NoError(t, err)
	defer r.Cancel()
	plan[0].EndMS = 1
	require.Equal(t, int64(1000), r.cfg.Masks[0].EndMS)
}

func TestMaskIntervalsConservativelyCoverFractionalTicks(t *testing.T) {
	r, _ := newTestRecorder(t)
	r.cfg.Masks = []Mask{{StartMS: 250, EndMS: 350, X: 0, Y: 0, Width: 64, Height: 48}}
	for tick := int64(0); tick < 5; tick++ {
		canvas := image.NewRGBA(image.Rect(0, 0, 64, 48))
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		r.applyMasks(canvas, tick)
		want := color.RGBA{255, 255, 255, 255}
		if tick == 2 || tick == 3 {
			want = color.RGBA{0, 0, 0, 255}
		}
		require.Equal(t, want, canvas.RGBAAt(10, 10))
	}
}

func TestCorruptScheduledOverlayFailsClosed(t *testing.T) {
	r, s := newTestRecorder(t)
	require.NoError(t, r.AddFrame(0, 0, jpegFrame(t, color.White)))
	require.NoError(t, r.AddAnnotation(0, 500, annotationPNG(t, image.Rect(0, 0, 10, 10))))
	require.NoError(t, os.WriteFile(r.annotations[0].path, []byte("corrupt"), 0600))
	_, err := r.Finalize(context.Background(), 1000, s)
	require.ErrorIs(t, err, ErrRender)
	_, err = s.Open(r.ID())
	require.Error(t, err)
	require.Equal(t, "failed", r.State())
}

func tone(samples int, amplitude float64) []byte {
	b := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(b[i*2:i*2+2], uint16(int16(amplitude*math.Sin(float64(i)*2*math.Pi*440/SampleRate))))
	}
	return b
}

func TestNarrationMixingOffsetsOverlapAndAudioVideoSync(t *testing.T) {
	r, s := newTestRecorder(t)
	require.NoError(t, r.AddFrame(0, 0, jpegFrame(t, color.Black)))
	clip := tone(4800, 8000)
	require.NoError(t, r.AddNarration(0, 300, 1000, clip))
	require.NoError(t, r.AddNarration(0, 300, 1000, clip))
	require.ErrorIs(t, r.AddNarration(0, 300, 500, clip), ErrConflict)
	require.ErrorIs(t, r.AddNarration(2, 500, 1000, clip), ErrConflict)
	require.ErrorIs(t, r.AddNarration(1, 500, 1001, clip), ErrInvalid)
	// Out-of-order timestamps are valid across different audio clips.
	require.NoError(t, r.AddNarration(1, 300, 500, clip))
	require.NoError(t, r.AddNarration(2, 200, 0, clip))
	path, err := r.mixNarration(context.Background(), 1000)
	require.NoError(t, err)
	mixed, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, mixed, 96000)
	for i := 0; i < 48000; i++ {
		sample := int16(binary.LittleEndian.Uint16(mixed[i*2 : i*2+2]))
		if i >= 14400 && i < 19200 {
			original := int16(binary.LittleEndian.Uint16(clip[(i-14400)*2 : (i-14400)*2+2]))
			require.Equal(t, int16(int(original)+int(original)/2), sample)
		} else {
			require.Zero(t, sample)
		}
	}
	require.NoError(t, os.Remove(path))
	_, err = r.Finalize(context.Background(), 1000, s)
	require.NoError(t, err)
	f, err := s.Open(r.ID())
	require.NoError(t, err)
	defer f.Close()
	raw, err := exec.Command("ffmpeg", "-v", "error", "-i", f.Name(), "-map", "0:a:0", "-f", "s16le", "-ar", "48000", "-ac", "1", "pipe:1").Output()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(raw), 96000)
	rms := func(start, end int) float64 {
		var sum float64
		for i := start; i < end; i++ {
			value := float64(int16(binary.LittleEndian.Uint16(raw[i*2 : i*2+2])))
			sum += value * value
		}
		return math.Sqrt(sum / float64(end-start))
	}
	require.Less(t, rms(4800, 9600), 10.0)
	require.Greater(t, rms(15360, 18240), 6000.0)
	require.Less(t, rms(28800, 33600), 10.0)
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,start_time,duration", "-of", "csv=p=0", f.Name()).Output()
	require.NoError(t, err)
	require.Contains(t, string(probe), "h264,0.000000,1.000000")
	require.Contains(t, string(probe), "aac,0.000000,1.000000")
}

func TestNarrationClippingAndInvalidSamples(t *testing.T) {
	r, _ := newTestRecorder(t)
	require.ErrorIs(t, r.AddNarration(0, 0, 1000, []byte{1}), ErrInvalid)
	require.ErrorIs(t, r.AddNarration(0, MaxDurationMS-1, 1000, make([]byte, 200)), ErrInvalid)
	require.ErrorIs(t, r.AddNarration(0, 0, 1000, make([]byte, MaxAudioChunkBytes+2)), ErrLimit)
	pcm := make([]byte, 96)
	for i := 0; i < 48; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:i*2+2], 30000)
	}
	require.NoError(t, r.AddNarration(0, 0, 1000, pcm))
	require.NoError(t, r.AddNarration(1, 0, 1000, pcm))
	path, err := r.mixNarration(context.Background(), 100)
	require.NoError(t, err)
	mixed, err := os.ReadFile(path)
	require.NoError(t, err)
	for i := 0; i < 48; i++ {
		require.Equal(t, uint16(32767), binary.LittleEndian.Uint16(mixed[i*2:i*2+2]))
	}
}
