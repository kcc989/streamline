// Package recording implements bounded, offline timestamped JPEG recordings.
// It never exposes intermediate frames or partial encoded output.
package recording

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	MaxFrameBytes  = 2 << 20
	MaxInputBytes  = 128 << 20
	MaxOutputBytes = 128 << 20
	MaxFrames      = 9000
	MaxDurationMS  = 300000
	Lifetime       = 15 * time.Minute
	RenderTimeout  = 5 * time.Minute
)

var (
	ErrInvalid  = errors.New("invalid recording input")
	ErrConflict = errors.New("recording state conflict")
	ErrBusy     = errors.New("recording operation in progress")
	ErrLimit    = errors.New("recording budget exceeded")
	ErrRender   = errors.New("recording render failed")
)

type Config struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	FPS    int    `json:"fps"`
	Masks  []Mask `json:"masks,omitempty"`
}

func (c Config) Validate() error {
	if c.Width < 2 || c.Height < 2 || c.Width > 1920 || c.Height > 1080 ||
		c.Width%2 != 0 || c.Height%2 != 0 || c.FPS < 1 || c.FPS > 30 {
		return ErrInvalid
	}
	if len(c.Masks) > MaxMasks {
		return ErrLimit
	}
	for _, mask := range c.Masks {
		if err := mask.validate(c); err != nil {
			return err
		}
	}
	return nil
}

type Artifact struct {
	ID         string `json:"id"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
}

// Sink commits a complete MP4 atomically. An error must publish nothing. Sink
// implementations are trusted server configuration, never browser input.
type Sink interface {
	Commit(context.Context, Artifact, io.Reader) error
}

type frame struct {
	timestamp int64
	hash      [32]byte
	path      string
}

// Recorder owns one spool directory and process. op rejects concurrent mutations
// instead of queuing unbounded work; mu protects state and cancellation.
type Recorder struct {
	mu          sync.Mutex
	op          sync.Mutex
	cfg         Config
	id          string
	dir         string
	ctx         context.Context
	cancel      context.CancelFunc
	timer       *time.Timer
	state       string
	frames      []frame
	annotations []frame
	narrations  []narration
	inputBytes  int64
	artifact    Artifact
	endMS       int64
	// Executable is fixed by the constructor, not a request field.
	ffmpeg string
}

func New(cfg Config) (*Recorder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "streamline-recording-")
	if err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg.Masks = append([]Mask(nil), cfg.Masks...)
	r := &Recorder{cfg: cfg, id: hex.EncodeToString(id), dir: dir, ctx: ctx, cancel: cancel,
		state: "accepting", ffmpeg: "ffmpeg"}
	r.timer = time.AfterFunc(Lifetime, r.Cancel)
	return r, nil
}

func (r *Recorder) ID() string { return r.id }

// Context is cancelled by Cancel or the recording lifetime deadline. Adapters
// use it to interrupt downloads before releasing the recording's resource slot.
func (r *Recorder) Context() context.Context { return r.ctx }

func (r *Recorder) State() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// AddFrame uses contiguous zero-based sequence numbers. Exact retries are
// idempotent, including after finalization; conflicting retries are rejected.
func (r *Recorder) AddFrame(sequence int, timestampMS int64, data []byte) error {
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
	if sequence < len(r.frames) {
		f := r.frames[sequence]
		r.mu.Unlock()
		if f.timestamp == timestampMS && f.hash == hash {
			return nil
		}
		return ErrConflict
	}
	if r.state != "accepting" || sequence != len(r.frames) ||
		(sequence == 0 && timestampMS != 0) ||
		(sequence > 0 && timestampMS <= r.frames[sequence-1].timestamp) {
		r.mu.Unlock()
		return ErrConflict
	}
	if len(r.frames) >= MaxFrames || r.inputBytes+int64(len(data)) > MaxInputBytes {
		r.mu.Unlock()
		return ErrLimit
	}
	r.mu.Unlock()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width != r.cfg.Width || cfg.Height != r.cfg.Height {
		return ErrInvalid
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		return ErrInvalid
	}
	path := filepath.Join(r.dir, fmt.Sprintf("frame-%d.jpg", sequence))
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		os.Remove(path)
		return ErrConflict
	}
	r.frames = append(r.frames, frame{timestamp: timestampMS, hash: hash, path: path})
	r.inputBytes += int64(len(data))
	return nil
}

// Finalize holds the last frame until endMS and samples the most recent frame
// at each CFR tick. endMS must fall on a tick (endMS*fps divisible by 1000).
// Request cancellation is terminal: callers must create a new recording.
func (r *Recorder) Finalize(ctx context.Context, endMS int64, sink Sink) (Artifact, error) {
	if !r.op.TryLock() {
		return Artifact{}, ErrBusy
	}
	defer r.op.Unlock()
	r.mu.Lock()
	if r.state == "ready" && r.endMS == endMS {
		a := r.artifact
		r.mu.Unlock()
		return a, nil
	}
	if r.state != "accepting" {
		r.mu.Unlock()
		return Artifact{}, ErrConflict
	}
	if sink == nil || len(r.frames) == 0 || endMS <= r.frames[len(r.frames)-1].timestamp ||
		endMS > MaxDurationMS || endMS <= 0 || endMS*int64(r.cfg.FPS)%1000 != 0 {
		r.mu.Unlock()
		return Artifact{}, ErrInvalid
	}
	r.state = "finalizing"
	r.mu.Unlock()
	renderCtx, cancel := context.WithTimeout(r.ctx, RenderTimeout)
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	path := filepath.Join(r.dir, "output.mp4")
	err := r.render(renderCtx, endMS, path)
	if err == nil {
		err = r.verify(renderCtx, endMS, path)
	}
	var a Artifact
	if err == nil {
		var f *os.File
		f, err = os.Open(path)
		if err == nil {
			var info os.FileInfo
			info, err = f.Stat()
			if err == nil && (info.Size() == 0 || info.Size() >= MaxOutputBytes) {
				err = ErrLimit
			}
			if err == nil {
				a = Artifact{ID: r.id, Bytes: info.Size(), DurationMS: endMS}
				err = renderCtx.Err()
				if err == nil {
					err = sink.Commit(renderCtx, a, f)
				}
			}
			f.Close()
		}
	}
	os.RemoveAll(r.dir)
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.narrations {
		r.narrations[index].data = nil
	}
	if err != nil {
		if r.state != "cancelled" {
			r.state = "failed"
		}
		return Artifact{}, fmt.Errorf("%w", ErrRender)
	}
	if r.ctx.Err() != nil {
		r.state = "cancelled"
		return Artifact{}, ErrRender
	}
	r.state = "ready"
	r.endMS = endMS
	r.artifact = a
	return a, nil
}

func (r *Recorder) render(ctx context.Context, endMS int64, path string) error {
	audioPath, err := r.mixNarration(ctx, endMS)
	if err != nil {
		return err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-threads", "1",
		"-f", "rawvideo", "-pixel_format", "rgb24", "-video_size", fmt.Sprintf("%dx%d", r.cfg.Width, r.cfg.Height),
		"-framerate", strconv.Itoa(r.cfg.FPS), "-i", "pipe:0"}
	if audioPath != "" {
		args = append(args, "-f", "s16le", "-ar", "48000", "-ac", "1", "-i", audioPath,
			"-map", "0:v:0", "-map", "1:a:0", "-c:a", "aac", "-b:a", "128k")
	} else {
		args = append(args, "-an")
	}
	args = append(args, "-c:v", "libx264", "-threads", "1",
		"-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p", "-movflags", "+faststart",
		"-t", fmt.Sprintf("%.3f", float64(endMS)/1000), "-fs", strconv.Itoa(MaxOutputBytes), path)
	cmd := exec.CommandContext(ctx, r.ffmpeg, args...)
	cmd.WaitDelay = 2 * time.Second
	// Discard decoder diagnostics: errors must never include media or paths.
	cmd.Stderr = io.Discard
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		pipe.Close()
		return err
	}
	writeErr := r.writeFrames(ctx, pipe, endMS)
	pipe.Close()
	if writeErr != nil {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if writeErr != nil {
		return writeErr
	}
	return waitErr
}

func (r *Recorder) writeFrames(ctx context.Context, w io.Writer, endMS int64) error {
	index := -1
	var source image.Image
	overlayIndex := -1
	var overlay image.Image
	canvas := image.NewRGBA(image.Rect(0, 0, r.cfg.Width, r.cfg.Height))
	rgb := make([]byte, r.cfg.Width*r.cfg.Height*3)
	for tick := int64(0); tick < endMS*int64(r.cfg.FPS)/1000; tick++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		timeMS := tick * 1000 / int64(r.cfg.FPS)
		next := index
		for next+1 < len(r.frames) && r.frames[next+1].timestamp <= timeMS {
			next++
		}
		if next != index {
			f, err := os.Open(r.frames[next].path)
			if err != nil {
				return err
			}
			source, err = jpeg.Decode(f)
			f.Close()
			if err != nil {
				return err
			}
			index = next
		}
		draw.Draw(canvas, canvas.Bounds(), source, image.Point{}, draw.Src)
		var err error
		overlayIndex, overlay, err = r.annotationAt(timeMS, overlayIndex, overlay)
		if err != nil {
			return err
		}
		if overlay != nil {
			draw.Draw(canvas, canvas.Bounds(), overlay, image.Point{}, draw.Over)
		}
		// Masks are applied last on every output tick, including held frames.
		// The encoder never receives masked source pixels or a partial mask.
		r.applyMasks(canvas, tick)
		for pixel := 0; pixel < r.cfg.Width*r.cfg.Height; pixel++ {
			copy(rgb[pixel*3:pixel*3+3], canvas.Pix[pixel*4:pixel*4+3])
		}
		if _, err := w.Write(rgb); err != nil {
			return err
		}
	}
	return nil
}

// Cancel is idempotent and interrupts an encoder before waiting for spool cleanup.
func (r *Recorder) Cancel() {
	r.mu.Lock()
	r.state = "cancelled"
	r.cancel()
	r.timer.Stop()
	r.mu.Unlock()
	r.op.Lock()
	defer r.op.Unlock()
	r.mu.Lock()
	for index := range r.narrations {
		r.narrations[index].data = nil
	}
	r.mu.Unlock()
	os.RemoveAll(r.dir)
}

// LocalSink stores one artifact in a server-owned directory. Open only returns
// a committed artifact; temporary files are private and removed on all failures.
type LocalSink struct{ dir string }

func NewLocalSink() (*LocalSink, error) {
	dir, err := os.MkdirTemp("", "streamline-artifacts-")
	if err != nil {
		return nil, err
	}
	return &LocalSink{dir: dir}, nil
}

func (s *LocalSink) Commit(ctx context.Context, a Artifact, source io.Reader) error {
	if !validID(a.ID) {
		return ErrInvalid
	}
	f, err := os.CreateTemp(s.dir, ".partial-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, io.LimitReader(source, MaxOutputBytes+1))
	if err == nil && (n != a.Bytes || n <= 0 || n >= MaxOutputBytes) {
		err = ErrLimit
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Link publishes atomically without overwriting an existing artifact.
	return os.Link(f.Name(), filepath.Join(s.dir, a.ID+".mp4"))
}

func validID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && id == hex.EncodeToString(b)
}

func (s *LocalSink) Open(id string) (*os.File, error) {
	if !validID(id) {
		return nil, ErrInvalid
	}
	return os.Open(filepath.Join(s.dir, id+".mp4"))
}

func (s *LocalSink) Close() error { return os.RemoveAll(s.dir) }
