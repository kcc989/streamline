package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cloudflare/streamline/container/recording"
)

type recordingSession struct {
	engine   *recording.Recorder
	sink     *recording.LocalSink
	timer    *time.Timer
	download sync.Mutex
}

// These are internal Container routes. The application must authenticate and
// authorize each request; recording IDs are fences, not identity credentials.
func (s *Server) handleRecording(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	path := r.URL.Path
	method := http.MethodPost
	if path == "/recordings/output" {
		method = http.MethodGet
	}
	if r.Method != method {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if path != "/recordings/output" && path != "/recordings/cancel" {
		if !s.recordingRequestMu.TryLock() {
			recordingError(w, recording.ErrBusy)
			return
		}
		defer s.recordingRequestMu.Unlock()
	}
	if path == "/recordings" {
		var cfg recording.Config
		if !decodeRecordingJSON(w, r, &cfg) {
			return
		}
		if err := cfg.Validate(); err != nil {
			recordingError(w, err)
			return
		}
		s.sessionMu.Lock()
		if s.recording != nil || s.httpHandler.IsSessionActive() {
			s.sessionMu.Unlock()
			recordingError(w, recording.ErrConflict)
			return
		}
		engine, err := recording.New(cfg)
		if err != nil {
			s.sessionMu.Unlock()
			recordingError(w, err)
			return
		}
		sink, err := recording.NewLocalSink()
		if err != nil {
			engine.Cancel()
			s.sessionMu.Unlock()
			recordingError(w, err)
			return
		}
		session := &recordingSession{engine: engine, sink: sink}
		s.recording = session
		session.timer = time.AfterFunc(recording.Lifetime, func() { s.clearRecording(session) })
		s.sessionMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"recording_id": engine.ID()})
		return
	}
	s.sessionMu.Lock()
	session := s.recording
	s.sessionMu.Unlock()
	if session == nil || session.engine.ID() != r.Header.Get("X-Streamline-Recording-ID") {
		recordingError(w, recording.ErrConflict)
		return
	}
	switch path {
	case "/recordings/frame", "/recordings/annotation", "/recordings/narration":
		contentType := "image/jpeg"
		if path == "/recordings/annotation" {
			contentType = "image/png"
		}
		if path == "/recordings/narration" {
			contentType = "application/octet-stream"
		}
		if r.Header.Get("Content-Type") != contentType {
			http.Error(w, "Unsupported recording content type", 415)
			return
		}
		sequence, err := strconv.Atoi(r.Header.Get("X-Frame-Sequence"))
		if err != nil {
			recordingError(w, recording.ErrInvalid)
			return
		}
		ts, err := strconv.ParseInt(r.Header.Get("X-Frame-Timestamp-MS"), 10, 64)
		if err != nil {
			recordingError(w, recording.ErrInvalid)
			return
		}
		body, err := recordingBody(w, r, recording.MaxFrameBytes)
		if err == nil {
			switch path {
			case "/recordings/frame":
				err = session.engine.AddFrame(sequence, ts, body)
			case "/recordings/annotation":
				err = session.engine.AddAnnotation(sequence, ts, body)
			case "/recordings/narration":
				gain, parseErr := strconv.Atoi(r.Header.Get("X-Narration-Gain"))
				if parseErr != nil {
					err = recording.ErrInvalid
				} else {
					err = session.engine.AddNarration(sequence, ts, gain, body)
				}
			}
		}
		if err != nil {
			recordingError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "/recordings/finalize":
		var body struct {
			EndMS int64 `json:"end_ms"`
		}
		if !decodeRecordingJSON(w, r, &body) {
			return
		}
		artifact, err := session.engine.Finalize(r.Context(), body.EndMS, session.sink)
		if err != nil {
			recordingError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(artifact)
	case "/recordings/output":
		if !session.download.TryLock() {
			recordingError(w, recording.ErrBusy)
			return
		}
		defer session.download.Unlock()
		controller := http.NewResponseController(w)
		controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		interrupted := make(chan struct{})
		stop := context.AfterFunc(session.engine.Context(), func() {
			controller.SetWriteDeadline(time.Now())
			close(interrupted)
		})
		defer func() {
			if !stop() {
				<-interrupted
			}
		}()
		if session.engine.State() != "ready" {
			recordingError(w, recording.ErrConflict)
			return
		}
		f, err := session.sink.Open(session.engine.ID())
		if err != nil {
			recordingError(w, recording.ErrConflict)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			recordingError(w, err)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Disposition", `attachment; filename="recording.mp4"`)
		http.ServeContent(w, r, "recording.mp4", info.ModTime(), f)
	case "/recordings/cancel":
		if r.ContentLength != 0 || r.TransferEncoding != nil {
			recordingError(w, recording.ErrInvalid)
			return
		}
		s.clearRecording(session)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) clearRecording(session *recordingSession) {
	// Keep the slot reserved while cleanup waits for the cancelled encoder.
	session.engine.Cancel()
	session.download.Lock()
	session.sink.Close()
	session.download.Unlock()
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.recording == session {
		session.timer.Stop()
		s.recording = nil
	}
}

func recordingBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	c := http.NewResponseController(w)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	data, err := io.ReadAll(r.Body)
	var large *http.MaxBytesError
	if errors.As(err, &large) {
		return nil, recording.ErrLimit
	}
	if err != nil {
		return nil, recording.ErrInvalid
	}
	return data, nil
}

func decodeRecordingJSON(w http.ResponseWriter, r *http.Request, value interface{}) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "Content-Type must be application/json", 415)
		return false
	}
	data, err := recordingBody(w, r, 64<<10)
	if err != nil {
		recordingError(w, err)
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil {
		recordingError(w, recording.ErrInvalid)
		return false
	}
	var extra interface{}
	if !errors.Is(d.Decode(&extra), io.EOF) {
		recordingError(w, recording.ErrInvalid)
		return false
	}
	return true
}

func recordingError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "Recording operation failed"
	switch {
	case errors.Is(err, recording.ErrInvalid):
		status = 400
		message = "Invalid recording input"
	case errors.Is(err, recording.ErrConflict):
		status = 409
		message = "Recording state conflict"
	case errors.Is(err, recording.ErrBusy):
		status = 429
		message = "Recording operation in progress"
		w.Header().Set("Retry-After", "1")
	case errors.Is(err, recording.ErrLimit):
		status = 413
		message = "Recording budget exceeded"
	}
	http.Error(w, message, status)
}
