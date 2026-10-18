package projects

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/justmike1/arbetern/internal/store"
)

const (
	maxTranscript   = 64 << 20
	transcriptType  = "application/x-ndjson"
	transcriptsPath = "/transcripts/"
)

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
