package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os/exec"
	"strconv"
	"time"
)

// Verify the muxed artifact before committing it. FFmpeg may stop successfully
// at -fs without encoding the complete requested timeline.
func (r *Recorder) verify(ctx context.Context, endMS int64, path string) error {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries",
		"stream=codec_name,width,height,nb_frames,duration,start_time:format=duration", "-of", "json", path)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stderr = io.Discard
	var output bytes.Buffer
	cmd.Stdout = &boundedProbeWriter{buffer: &output}
	if err := cmd.Run(); err != nil {
		return ErrRender
	}
	var metadata struct {
		Streams []struct {
			Codec    string `json:"codec_name"`
			Width    int    `json:"width"`
			Height   int    `json:"height"`
			Frames   string `json:"nb_frames"`
			Duration string `json:"duration"`
			Start    string `json:"start_time"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(output.Bytes(), &metadata) != nil {
		return ErrRender
	}
	count := 1
	if len(r.narrations) > 0 {
		count = 2
	}
	if len(metadata.Streams) != count {
		return ErrRender
	}
	video := metadata.Streams[0]
	frames, err := strconv.ParseInt(video.Frames, 10, 64)
	if err != nil || frames != endMS*int64(r.cfg.FPS)/1000 || video.Codec != "h264" ||
		video.Width != r.cfg.Width || video.Height != r.cfg.Height || !near(video.Start, 0) ||
		!near(video.Duration, float64(endMS)/1000) || !near(metadata.Format.Duration, float64(endMS)/1000) {
		return ErrRender
	}
	if count == 2 && (metadata.Streams[1].Codec != "aac" || !near(metadata.Streams[1].Start, 0) ||
		!near(metadata.Streams[1].Duration, float64(endMS)/1000)) {
		return ErrRender
	}
	return nil
}

func near(value string, target float64) bool {
	x, err := strconv.ParseFloat(value, 64)
	return err == nil && !math.IsNaN(x) && !math.IsInf(x, 0) && math.Abs(x-target) <= 0.001
}

type boundedProbeWriter struct{ buffer *bytes.Buffer }

func (w *boundedProbeWriter) Write(p []byte) (int, error) {
	if len(p)+w.buffer.Len() > 64<<10 {
		return 0, ErrLimit
	}
	return w.buffer.Write(p)
}
