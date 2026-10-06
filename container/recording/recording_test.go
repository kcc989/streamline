package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func jpegFrame(t *testing.T, c color.Color) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, 64, 48))
	draw.Draw(m, m.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, m, &jpeg.Options{Quality: 100}))
	return b.Bytes()
}

func newTestRecorder(t *testing.T) (*Recorder, *LocalSink) {
	t.Helper()
	r, err := New(Config{Width: 64, Height: 48, FPS: 10})
	require.NoError(t, err)
	s, err := NewLocalSink()
	require.NoError(t, err)
	t.Cleanup(func() { r.Cancel(); s.Close() })
	return r, s
}

func TestFrameRetriesOrderingAndBudgets(t *testing.T) {
	r, _ := newTestRecorder(t)
	a := jpegFrame(t, color.RGBA{R: 255, A: 255})
	require.ErrorIs(t, r.AddFrame(0, 10, a), ErrConflict)
	require.NoError(t, r.AddFrame(0, 0, a))
	require.NoError(t, r.AddFrame(0, 0, a))
	require.ErrorIs(t, r.AddFrame(0, 1, a), ErrConflict)
	require.ErrorIs(t, r.AddFrame(2, 100, a), ErrConflict)
	require.ErrorIs(t, r.AddFrame(1, 0, a), ErrConflict)
	require.ErrorIs(t, r.AddFrame(1, 100, []byte("not JPEG")), ErrInvalid)
	require.ErrorIs(t, r.AddFrame(1, 100, make([]byte, MaxFrameBytes+1)), ErrLimit)
	r.op.Lock()
	require.ErrorIs(t, r.AddFrame(1, 100, a), ErrBusy)
	r.op.Unlock()
	r.inputBytes = MaxInputBytes
	require.ErrorIs(t, r.AddFrame(1, 100, a), ErrLimit)
}

func TestDecodedMP4PreservesTimestampHoldsAndFinalization(t *testing.T) {
	r, s := newTestRecorder(t)
	require.NoError(t, r.AddFrame(0, 0, jpegFrame(t, color.RGBA{R: 255, A: 255})))
	require.NoError(t, r.AddFrame(1, 350, jpegFrame(t, color.RGBA{B: 255, A: 255})))
	_, err := s.Open(r.ID())
	require.Error(t, err)
	_, err = r.Finalize(context.Background(), 951, s)
	require.ErrorIs(t, err, ErrInvalid)
	a, err := r.Finalize(context.Background(), 1000, s)
	require.NoError(t, err)
	require.Equal(t, int64(1000), a.DurationMS)
	require.Equal(t, "ready", r.State())
	_, err = os.Stat(r.dir)
	require.True(t, os.IsNotExist(err))
	a2, err := r.Finalize(context.Background(), 1000, s)
	require.NoError(t, err)
	require.Equal(t, a, a2)
	_, err = r.Finalize(context.Background(), 900, s)
	require.ErrorIs(t, err, ErrConflict)
	f, err := s.Open(r.ID())
	require.NoError(t, err)
	defer f.Close()
	decoded, err := exec.Command("ffmpeg", "-v", "error", "-i", f.Name(), "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1").Output()
	require.NoError(t, err)
	require.Len(t, decoded, 64*48*3*10)
	for i := 0; i < 10; i++ {
		p := decoded[i*64*48*3:]
		if i < 4 {
			require.Greater(t, p[0], byte(240))
			require.Less(t, p[2], byte(10))
		} else {
			require.Greater(t, p[2], byte(240))
			require.Less(t, p[0], byte(10))
		}
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_name,nb_frames", "-of", "json", f.Name()).Output()
	require.NoError(t, err)
	var metadata struct {
		Format  struct{ Duration string }
		Streams []struct {
			CodecName string `json:"codec_name"`
			Frames    string `json:"nb_frames"`
		}
	}
	require.NoError(t, json.Unmarshal(probe, &metadata))
	require.Equal(t, "1.000000", metadata.Format.Duration)
	require.Equal(t, "h264", metadata.Streams[0].CodecName)
	require.Equal(t, "10", metadata.Streams[0].Frames)
}

type failingSink struct{}

func (failingSink) Commit(context.Context, Artifact, io.Reader) error { return io.ErrClosedPipe }

func TestFailuresAndCancellationPublishNothing(t *testing.T) {
	for _, mode := range []string{"encoder", "sink", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			r, s := newTestRecorder(t)
			require.NoError(t, r.AddFrame(0, 0, jpegFrame(t, color.Black)))
			var sink Sink = s
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "encoder":
				r.ffmpeg = "/nonexistent/ffmpeg"
			case "sink":
				sink = failingSink{}
			case "cancel":
				cancel()
			}
			_, err := r.Finalize(ctx, 1000, sink)
			require.ErrorIs(t, err, ErrRender)
			_, err = s.Open(r.ID())
			require.Error(t, err)
			_, err = os.Stat(r.dir)
			require.True(t, os.IsNotExist(err))
			require.Equal(t, "failed", r.State())
			r.Cancel()
			r.Cancel()
			require.ErrorIs(t, r.AddFrame(0, 0, jpegFrame(t, color.Black)), ErrConflict)
		})
	}
}

func TestCancellationInterruptsBlockedEncoderAndCleansSpool(t *testing.T) {
	r, s := newTestRecorder(t)
	path := filepath.Join(t.TempDir(), "ffmpeg")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 30\n"), 0700))
	r.ffmpeg = path
	require.NoError(t, r.AddFrame(0, 0, jpegFrame(t, color.White)))
	done := make(chan error, 1)
	go func() { _, err := r.Finalize(context.Background(), 10000, s); done <- err }()
	require.Eventually(t, func() bool { return r.State() == "finalizing" }, time.Second, time.Millisecond)
	r.Cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrRender)
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not interrupt encoder")
	}
	require.Equal(t, "cancelled", r.State())
	_, err := os.Stat(r.dir)
	require.True(t, os.IsNotExist(err))
	_, err = s.Open(r.ID())
	require.Error(t, err)
}
