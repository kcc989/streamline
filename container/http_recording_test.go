package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/streamline/container/recording"

	"github.com/stretchr/testify/require"
)

func recordingRequest(t *testing.T, s *Server, method, path, id, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("X-Streamline-Recording-ID", id)
	r.Header.Set("X-Frame-Sequence", "0")
	r.Header.Set("X-Frame-Timestamp-MS", "0")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestRecordingHTTPWorkflowAndIsolation(t *testing.T) {
	s := NewServer()
	w := recordingRequest(t, s, "POST", "/recordings", "", "application/json", []byte(`{"width":64,"height":48,"fps":10}`))
	require.Equal(t, http.StatusCreated, w.Code)
	var created struct {
		ID string `json:"recording_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)
	session := s.recording
	t.Cleanup(func() { s.clearRecording(session) })
	w = recordingRequest(t, s, "GET", "/recordings/output", created.ID, "", nil)
	require.Equal(t, 409, w.Code)
	w = recordingRequest(t, s, "POST", "/recordings/finalize", "stale", "application/json", []byte(`{"end_ms":1000}`))
	require.Equal(t, 409, w.Code)
	s.recordingRequestMu.Lock()
	w = recordingRequest(t, s, "POST", "/recordings/frame", created.ID, "image/jpeg", nil)
	s.recordingRequestMu.Unlock()
	require.Equal(t, 429, w.Code)
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	canvas := image.NewRGBA(image.Rect(0, 0, 64, 48))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	var jpegData bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegData, canvas, nil))
	for i := 0; i < 2; i++ {
		w = recordingRequest(t, s, "POST", "/recordings/frame", created.ID, "image/jpeg", jpegData.Bytes())
		require.Equal(t, 204, w.Code)
	}
	w = recordingRequest(t, s, "POST", "/start", "", "application/json", []byte(`{"input":{"type":"webcam"},"output":{"mode":"websocket"},"pipeline":[{"op":"encode","params":{"codec":"h264"}}]}`))
	require.Equal(t, 409, w.Code)
	for i := 0; i < 2; i++ {
		w = recordingRequest(t, s, "POST", "/recordings/finalize", created.ID, "application/json", []byte(`{"end_ms":1000}`))
		require.Equal(t, 200, w.Code, w.Body.String())
	}
	var first []byte
	for i := 0; i < 2; i++ {
		w = recordingRequest(t, s, "GET", "/recordings/output", created.ID, "", nil)
		require.Equal(t, 200, w.Code)
		require.Equal(t, "video/mp4", w.Header().Get("Content-Type"))
		if i == 0 {
			first = append([]byte(nil), w.Body.Bytes()...)
		} else {
			require.Equal(t, first, w.Body.Bytes())
		}
	}
	w = recordingRequest(t, s, "POST", "/recordings/cancel", created.ID, "", nil)
	require.Equal(t, 204, w.Code)
	require.Nil(t, s.recording)
	w = recordingRequest(t, s, "GET", "/recordings/output", created.ID, "", nil)
	require.Equal(t, 409, w.Code)
}

func TestRecordingRejectsInvalidConfigWithoutReservingSlot(t *testing.T) {
	for _, cfg := range []string{`{"width":64,"height":48,"fps":10,"command":"anything"}`, `null`,
		`{"width":64000,"height":48,"fps":10}`, `{"width":64,"height":48,"fps":10} {}`} {
		s := NewServer()
		w := recordingRequest(t, s, "POST", "/recordings", "", "application/json", []byte(cfg))
		require.Equal(t, 400, w.Code)
		require.Nil(t, s.recording)
	}
}

type blockedRecordingDownload struct {
	header        http.Header
	entered       chan struct{}
	interrupted   chan struct{}
	writeOnce     sync.Once
	interruptOnce sync.Once
}

func (w *blockedRecordingDownload) Header() http.Header { return w.header }
func (w *blockedRecordingDownload) WriteHeader(int)     {}
func (w *blockedRecordingDownload) Write(data []byte) (int, error) {
	w.writeOnce.Do(func() { close(w.entered) })
	<-w.interrupted
	return 0, os.ErrDeadlineExceeded
}
func (w *blockedRecordingDownload) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		w.interruptOnce.Do(func() { close(w.interrupted) })
	}
	return nil
}

func TestRecordingCancellationInterruptsDownloadBeforeReleasingSlot(t *testing.T) {
	s := NewServer()
	r, err := recording.New(recording.Config{Width: 64, Height: 48, FPS: 10})
	require.NoError(t, err)
	sink, err := recording.NewLocalSink()
	require.NoError(t, err)
	session := &recordingSession{engine: r, sink: sink}
	session.timer = time.NewTimer(time.Hour)
	s.recording = session
	t.Cleanup(func() { s.clearRecording(session) })
	var frame bytes.Buffer
	require.NoError(t, jpeg.Encode(&frame, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil))
	require.NoError(t, r.AddFrame(0, 0, frame.Bytes()))
	_, err = r.Finalize(t.Context(), 1000, sink)
	require.NoError(t, err)
	w := &blockedRecordingDownload{header: make(http.Header), entered: make(chan struct{}), interrupted: make(chan struct{})}
	request := httptest.NewRequest("GET", "/recordings/output", nil)
	request.Header.Set("X-Streamline-Recording-ID", r.ID())
	downloadDone := make(chan struct{})
	go func() { s.ServeHTTP(w, request); close(downloadDone) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("download did not begin")
	}
	cleanupDone := make(chan struct{})
	go func() { s.clearRecording(session); close(cleanupDone) }()
	select {
	case <-cleanupDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not interrupt download")
	}
	<-downloadDone
	s.sessionMu.Lock()
	require.Nil(t, s.recording)
	s.sessionMu.Unlock()
	_, err = sink.Open(r.ID())
	require.Error(t, err)
}
