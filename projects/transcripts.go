package projects

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/redact"
	"github.com/justmike1/arbetern/internal/store"
)

const (
	maxTranscript         = 64 << 20
	transcriptType        = "application/x-ndjson"
	transcriptsPath       = "/transcripts/"
	transcriptViewWait    = 30 * time.Second
	transcriptViewTimeout = 2 * time.Minute
	sessionTokenMask      = "[session token]"
)

// transcriptViews bounds the console reads in flight, since each holds a whole line of up to maxTranscript in memory.
var transcriptViews = make(chan struct{}, 2)

func transcriptKey(project, task string) string {
	return StatePrefix + project + transcriptsPath + task + ".jsonl"
}

func (g *gateway) handlePutTranscript(w http.ResponseWriter, req *http.Request) {
	s := sessionFrom(req)
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxTranscript))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "the transcript is larger than "+strconv.Itoa(maxTranscript>>20)+" MiB")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "could not read the transcript")
		return
	case len(body) == 0:
		writeError(w, http.StatusBadRequest, "the transcript is empty")
		return
	}
	// A session that printed its own token, as git config does, leaves it out of what others read.
	if token, ok := sessionToken(req.Header); ok {
		body = bytes.ReplaceAll(body, []byte(token), []byte(sessionTokenMask))
	}
	p, taskID := s.project, s.task.ID
	if _, err := g.r.b.Put(req.Context(), transcriptKey(p.ID, taskID), body, transcriptType, store.Condition{}); err != nil {
		log.Printf("[projects] %s task %s: storing the transcript: %v", p.ID, taskID, err)
		writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
		return
	}
	now, size := time.Now().UTC(), len(body)
	if _, err := g.apply(req.Context(), s, func(l *Ledger) bool { return l.setTranscript(taskID, size, now) }); err != nil {
		log.Printf("[projects] %s task %s: recording the transcript: %v", p.ID, taskID, err)
		writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *gateway) handleGetTranscript(w http.ResponseWriter, req *http.Request) {
	s := sessionFrom(req)
	g.r.serveTranscript(w, req, s.project.ID, s.task.ID)
}

func (r *Registry) serveTranscript(w http.ResponseWriter, req *http.Request, id, task string) {
	data, _, err := r.b.Get(req.Context(), transcriptKey(id, task))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no transcript is stored for this task")
		return
	case err != nil:
		log.Printf("[projects] %s task %s: reading the transcript: %v", id, task, err)
		writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
		return
	}
	w.Header().Set("Content-Type", transcriptType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// serveTranscriptView is the console's copy: tool results hold what the session read as it came, so every string is redacted.
func (r *Registry) serveTranscriptView(w http.ResponseWriter, req *http.Request, id, task string) {
	wait := time.NewTimer(transcriptViewWait)
	defer wait.Stop()
	select {
	case transcriptViews <- struct{}{}:
		defer func() { <-transcriptViews }()
	case <-wait.C:
		writeError(w, http.StatusServiceUnavailable, "other transcripts are being read; try again in a moment")
		return
	case <-req.Context().Done():
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), transcriptViewTimeout)
	defer cancel()
	body, err := r.b.Stream(ctx, transcriptKey(id, task))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no transcript is stored for this task")
		return
	case err != nil:
		log.Printf("[projects] %s task %s: reading the transcript: %v", id, task, err)
		writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
		return
	}
	defer func() { _ = body.Close() }()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(transcriptViewTimeout))
	w.Header().Set("Content-Type", transcriptType)
	w.Header().Set("Cache-Control", "no-store")
	rd := bufio.NewReader(io.LimitReader(body, maxTranscript))
	for {
		line, err := rd.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			if _, werr := w.Write(append(scrubLine(line), '\n')); werr != nil {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("[projects] %s task %s: reading the transcript: %v", id, task, err)
			}
			return
		}
	}
}

func scrubLine(line []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if enc.Encode(scrubValue(v)) == nil {
			return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
		}
	}
	return []byte(redact.Text(string(line)))
}

func scrubValue(v any) any {
	switch t := v.(type) {
	case string:
		return redact.Text(t)
	case []any:
		for i, x := range t {
			t[i] = scrubValue(x)
		}
	case map[string]any:
		for k, x := range t {
			t[k] = scrubValue(x)
		}
	}
	return v
}

// pruneTranscripts deletes the transcripts of tasks the ledger no longer keeps and reports whether none is left.
func (r *Registry) pruneTranscripts(ctx context.Context, id string, led *Ledger) bool {
	objs, err := r.b.List(ctx, StatePrefix+id+transcriptsPath)
	if err != nil {
		log.Printf("[projects] listing the transcripts of %s: %v", id, err)
		return false
	}
	ok := true
	for _, o := range objs {
		if led.task(strings.TrimSuffix(path.Base(o.Key), ".jsonl")) != nil {
			continue
		}
		if err := r.b.Delete(ctx, o.Key, ""); err != nil {
			log.Printf("[projects] deleting %s: %v", o.Key, err)
			ok = false
		}
	}
	return ok
}

func (r *Registry) deleteTranscripts(ctx context.Context, id string) {
	objs, err := r.b.List(ctx, StatePrefix+id+transcriptsPath)
	if err != nil {
		log.Printf("[projects] listing the transcripts of %s: %v", id, err)
		return
	}
	for _, o := range objs {
		if err := r.b.Delete(ctx, o.Key, ""); err != nil {
			log.Printf("[projects] deleting %s: %v", o.Key, err)
		}
	}
}
