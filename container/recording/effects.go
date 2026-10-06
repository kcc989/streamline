package recording

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"

	"github.com/cloudflare/streamline/container/media"
)

const (
	MaxMasks           = 64
	MaxAnnotations     = 1000
	MaxNarrations      = 16
	MaxNarrationBytes  = 32 << 20
	MaxAudioChunkBytes = 2 << 20
	SampleRate         = 48000
)

// Mask covers a pixel rectangle during [StartMS, EndMS). Only opaque black is
// supported. Every output tick overlapping the interval is masked. Bounds are
// rounded outward to 16px blocks with one guard block
// to isolate codec/chroma edges; a mask may cover more than its requested rect.
type Mask struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
	X       int   `json:"x"`
	Y       int   `json:"y"`
	Width   int   `json:"width"`
	Height  int   `json:"height"`
}

func (m Mask) validate(c Config) error {
	if m.StartMS < 0 || m.EndMS <= m.StartMS || m.EndMS > MaxDurationMS ||
		m.X < 0 || m.Y < 0 || m.X >= c.Width || m.Y >= c.Height || m.Width < 1 || m.Height < 1 ||
		m.Width > c.Width-m.X || m.Height > c.Height-m.Y {
		return ErrInvalid
	}
	return nil
}

func (r *Recorder) applyMasks(dst *image.RGBA, tick int64) {
	for _, m := range r.cfg.Masks {
		if m.StartMS*int64(r.cfg.FPS) >= (tick+1)*1000 || m.EndMS*int64(r.cfg.FPS) <= tick*1000 {
			continue
		}
		bounds := image.Rect(m.X/16*16-16, m.Y/16*16-16,
			(m.X+m.Width+15)/16*16+16, (m.Y+m.Height+15)/16*16+16)
		draw.Draw(dst, bounds.Intersect(dst.Bounds()), image.NewUniform(color.Black), image.Point{}, draw.Src)
	}
}

// AddAnnotation schedules the same full-canvas PNG snapshots accepted by the
// live annotation API. Callers draw cursors, click effects, and callouts there;
// a transparent snapshot clears the overlay. No custom drawing language.
func (r *Recorder) AddAnnotation(sequence int, timestampMS int64, data []byte) error {
	if !r.op.TryLock() {
		return ErrBusy
	}
	defer r.op.Unlock()
	if sequence < 0 || timestampMS < 0 || timestampMS >= MaxDurationMS || len(data) == 0 {
		return ErrInvalid
	}
	if len(data) > MaxFrameBytes {
		return ErrLimit
	}
	hash := sha256.Sum256(data)
	r.mu.Lock()
	if r.state == "cancelled" || r.state == "failed" {
		r.mu.Unlock()
		return ErrConflict
	}
	if sequence < len(r.annotations) {
		f := r.annotations[sequence]
		r.mu.Unlock()
		if f.timestamp == timestampMS && f.hash == hash {
			return nil
		}
		return ErrConflict
	}
	if r.state != "accepting" || sequence != len(r.annotations) ||
		(sequence > 0 && timestampMS <= r.annotations[sequence-1].timestamp) {
		r.mu.Unlock()
		return ErrConflict
	}
	if sequence >= MaxAnnotations || r.inputBytes+int64(len(data)) > MaxInputBytes {
		r.mu.Unlock()
		return ErrLimit
	}
	r.mu.Unlock()
	if _, err := media.DecodeAnnotationPNG(data, r.cfg.Width, r.cfg.Height); err != nil {
		return ErrInvalid
	}
	path := filepath.Join(r.dir, fmt.Sprintf("annotation-%d.png", sequence))
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		os.Remove(path)
		return ErrConflict
	}
	r.annotations = append(r.annotations, frame{timestamp: timestampMS, hash: hash, path: path})
	r.inputBytes += int64(len(data))
	return nil
}

func (r *Recorder) annotationAt(timestampMS int64, index int, current image.Image) (int, image.Image, error) {
	next := index
	for next+1 < len(r.annotations) && r.annotations[next+1].timestamp <= timestampMS {
		next++
	}
	if next == index {
		return index, current, nil
	}
	data, err := os.ReadFile(r.annotations[next].path)
	if err != nil {
		return index, nil, err
	}
	img, err := media.DecodeAnnotationPNG(data, r.cfg.Width, r.cfg.Height)
	return next, img, err
}

type narration struct {
	startMS int64
	gain    int // 0..1000, fixed-point linear gain
	data    []byte
	hash    [32]byte
}

// AddNarration ingests mono signed PCM16 little-endian at 48kHz. Clip sequence
// is contiguous; clips may overlap or arrive with earlier start timestamps.
// Clip duration is limited to 2MiB; total audio is limited to 32MiB.
func (r *Recorder) AddNarration(sequence int, startMS int64, gain int, data []byte) error {
	if !r.op.TryLock() {
		return ErrBusy
	}
	defer r.op.Unlock()
	if sequence < 0 || startMS < 0 || gain < 0 || gain > 1000 || len(data) == 0 || len(data)%2 != 0 ||
		startMS >= MaxDurationMS || int64(len(data)/2)+startMS*48 > MaxDurationMS*48 {
		return ErrInvalid
	}
	if len(data) > MaxAudioChunkBytes {
		return ErrLimit
	}
	hash := sha256.Sum256(data)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == "cancelled" || r.state == "failed" {
		return ErrConflict
	}
	if sequence < len(r.narrations) {
		n := r.narrations[sequence]
		if n.startMS == startMS && n.gain == gain && n.hash == hash {
			return nil
		}
		return ErrConflict
	}
	if r.state != "accepting" || sequence != len(r.narrations) {
		return ErrConflict
	}
	total := len(data)
	for _, clip := range r.narrations {
		total += len(clip.data)
	}
	if sequence >= MaxNarrations || total > MaxNarrationBytes || r.inputBytes+int64(len(data)) > MaxInputBytes {
		return ErrLimit
	}
	r.narrations = append(r.narrations, narration{startMS: startMS, gain: gain, data: bytes.Clone(data), hash: hash})
	r.inputBytes += int64(len(data))
	return nil
}

// mixNarration produces exactly endMS of PCM. Overlaps sum at their sample
// offsets and saturate at int16 bounds; silence fills gaps. Video has no source
// audio. Audio is provided by the application without credentials or URLs.
func (r *Recorder) mixNarration(ctx context.Context, endMS int64) (string, error) {
	if len(r.narrations) == 0 {
		return "", nil
	}
	for _, n := range r.narrations {
		if n.startMS*48+int64(len(n.data)/2) > endMS*48 {
			return "", ErrInvalid
		}
	}
	path := filepath.Join(r.dir, "mixed.pcm")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	const blockSamples = 4800
	block := make([]byte, blockSamples*2)
	for offset := int64(0); offset < endMS*48; offset += blockSamples {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count := int(min(int64(blockSamples), endMS*48-offset))
		for i := 0; i < count; i++ {
			var value int64
			for _, n := range r.narrations {
				index := offset + int64(i) - n.startMS*48
				if index >= 0 && index < int64(len(n.data)/2) {
					value += int64(int16(binary.LittleEndian.Uint16(n.data[index*2:index*2+2]))) * int64(n.gain) / 1000
				}
			}
			value = max(-32768, min(32767, value))
			binary.LittleEndian.PutUint16(block[i*2:i*2+2], uint16(int16(value)))
		}
		if _, err := f.Write(block[:count*2]); err != nil {
			return "", err
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, nil
}
