// Package proxy exposes the credential pool as a single local endpoint that
// Claude Code and Codex can be pointed at with nothing but configuration.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bojieli/agentswap/internal/config"
	"github.com/bojieli/agentswap/internal/engine"
	"github.com/bojieli/agentswap/internal/store"
	"github.com/bojieli/agentswap/internal/supervisor"
)

// maxBodyBytes caps a buffered request. The body has to be held in memory to
// be replayable, and replay is what makes rotation invisible to the client.
// The limit is generous: even a filled million-token context is a few MB.
const maxBodyBytes = 64 << 20

// Lane path prefixes. One port serves both so there is a single process to
// run and a single address to configure.
const (
	prefixAnthropic = "/anthropic"
	prefixOpenAI    = "/openai"
	prefixAdmin     = "/_agentswap"
)

// Server is the HTTP front end.
type Server struct {
	Engine    *engine.Engine
	Store     *store.Store
	Config    config.Config
	Keepalive KeepaliveMode
	Log       *slog.Logger

	// ConfigDir is where handoff tickets are written so `agentswap run` can
	// resume the session once quota returns. Empty disables ticket writing.
	ConfigDir string
}

// Handler builds the routing mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(prefixAdmin+"/health", s.handleHealth)
	mux.HandleFunc(prefixAdmin+"/status", s.handleStatus)
	mux.HandleFunc(prefixAnthropic+"/", s.laneHandler(store.LaneAnthropic, prefixAnthropic))
	mux.HandleFunc(prefixOpenAI+"/", s.laneHandler(store.LaneOpenAI, prefixOpenAI))
	mux.HandleFunc("/", s.handleUnrouted)
	return s.guardHost(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

// AccountStatus is one row of `agentswap status`.
type AccountStatus struct {
	ID       string         `json:"id"`
	Label    string         `json:"label"`
	Lane     store.LaneID   `json:"lane"`
	Kind     store.Kind     `json:"kind"`
	Enabled  bool           `json:"enabled"`
	Priority int            `json:"priority"`
	Health   store.Health   `json:"health"`
	Windows  []store.Window `json:"windows,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	var out []AccountStatus
	for _, a := range s.Store.All() {
		h := s.Store.Health(a.ID)
		out = append(out, AccountStatus{
			ID: a.ID, Label: a.Label, Lane: a.Lane, Kind: a.Kind,
			Enabled: a.Enabled, Priority: a.Priority,
			Health: h, Windows: h.Windows,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"accounts": out})
}

func (s *Server) handleUnrouted(w http.ResponseWriter, r *http.Request) {
	s.Log.Warn("request to an unrouted path", "path", r.URL.Path)
	writeJSONError(w, http.StatusNotFound, "not_found", fmt.Sprintf(
		"agentswap serves %s and %s; got %q. Run `agentswap install` to configure your CLI.",
		prefixAnthropic, prefixOpenAI, r.URL.Path))
}

func (s *Server) laneHandler(laneID store.LaneID, prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large",
					fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes))
				return
			}
			// The client went away mid-upload. Reporting that back as an
			// oversized body sends whoever reads the log hunting for a limit
			// that was never reached.
			s.Log.Debug("could not read request body", "err", err)
			writeJSONError(w, http.StatusBadRequest, "bad_request",
				"could not read the request body: "+err.Error())
			return
		}

		// Re-root the path onto the upstream: "/anthropic/v1/messages"
		// becomes "/v1/messages", which the lane then joins to its own base.
		upstreamPath := strings.TrimPrefix(r.URL.Path, prefix)
		if upstreamPath == "" {
			upstreamPath = "/"
		}
		out := r.Clone(ctx)
		out.URL.Path = upstreamPath

		waiter := newStreamWaiter(w, s.Keepalive, s.Config.Park)
		start := time.Now()

		res, err := s.Engine.Execute(ctx, laneID, out, body, waiter)
		if err != nil {
			s.writeExecuteError(w, waiter, laneID, err)
			return
		}
		defer res.Response.Body.Close()

		s.Log.Info("served",
			"lane", laneID, "account", res.Account.Display(),
			"status", res.Response.StatusCode, "attempts", res.Attempts,
			"took", time.Since(start).Round(time.Millisecond))

		s.relay(w, waiter, res.Response)
	}
}

// relay streams the upstream response to the client, flushing as it goes so
// tokens appear as they are produced rather than at the end.
func (s *Server) relay(w http.ResponseWriter, waiter *streamWaiter, resp *http.Response) {
	rc := http.NewResponseController(w)

	if waiter.Committed() {
		// We already sent 200 + text/event-stream while parked, so the status
		// line is spent. Only the body can be forwarded now.
		s.pump(w, rc, resp.Body)
		return
	}

	engine.CopyHeader(w.Header(), resp.Header)
	// Length no longer describes what we are about to write once we stream it
	// out chunk by chunk.
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	_ = rc.Flush()

	s.pump(w, rc, resp.Body)
}

func (s *Server) pump(w http.ResponseWriter, rc *http.ResponseController, body io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				s.Log.Debug("client hung up mid-stream", "err", werr)
				return
			}
			// No write deadline while streaming: a model can think for minutes
			// between tokens and that is not a stall.
			_ = rc.SetWriteDeadline(time.Time{})
			_ = rc.Flush()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.Log.Warn("upstream stream ended early", "err", err)
			}
			return
		}
	}
}

// writeExecuteError turns an engine failure into something the client can act
// on, respecting whether we have already committed to a status line.
func (s *Server) writeExecuteError(w http.ResponseWriter, waiter *streamWaiter, laneID store.LaneID, err error) {
	if errors.Is(err, io.EOF) {
		return
	}
	// The client hung up, or its own timeout fired. There is nobody left to
	// write to, and logging it as a failure buries the real ones — a user who
	// presses Ctrl-C mid-request has not hit a bug.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.Log.Debug("client went away before a response was ready", "lane", laneID, "err", err)
		return
	}

	var tooLong *engine.ErrParkTooLong
	switch {
	case errors.As(err, &tooLong):
		s.Log.Warn("pool exhausted beyond max hold", "lane", laneID, "until", tooLong.Until)
		s.writeTicket(laneID, tooLong.Until)
		retryAfter := int(time.Until(tooLong.Until).Seconds())
		if retryAfter < 1 {
			retryAfter = 1
		}
		if !waiter.Committed() {
			w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
		}
		s.fail(w, waiter, http.StatusServiceUnavailable, "quota_exhausted", fmt.Sprintf(
			"every %s account is out of quota until %s. Run `agentswap run -- <your command>` to have the session resumed automatically.",
			laneID, tooLong.Until.Format(time.RFC3339)))

	case errors.Is(err, engine.ErrNoAccounts):
		s.fail(w, waiter, http.StatusServiceUnavailable, "no_accounts", fmt.Sprintf(
			"no usable %s account: %v. Run `agentswap import`, or `agentswap add-key %s --key ...`.", laneID, err, laneID))

	default:
		s.Log.Error("request failed", "lane", laneID, "err", err)
		s.fail(w, waiter, http.StatusBadGateway, "upstream_error", err.Error())
	}
}

// fail reports an error either as an HTTP status or, if the status line was
// already spent keeping the connection alive, as an in-band SSE error event.
func (s *Server) fail(w http.ResponseWriter, waiter *streamWaiter, status int, code, msg string) {
	if !waiter.Committed() {
		writeJSONError(w, status, code, msg)
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": code, "message": msg},
	})
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	_ = http.NewResponseController(w).Flush()
}

// writeTicket records the handoff for `agentswap run` to act on. A failure
// here must not change what the client is told: they still get their 503, and
// an error about failing to record an error helps nobody.
func (s *Server) writeTicket(laneID store.LaneID, until time.Time) {
	if s.ConfigDir == "" {
		return
	}
	err := supervisor.WriteTicket(s.ConfigDir, supervisor.Ticket{
		Lane: laneID, Until: until, WrittenAt: time.Now(),
	})
	if err != nil {
		s.Log.Warn("could not write resume ticket", "lane", laneID, "err", err)
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": code, "message": msg},
	})
}
