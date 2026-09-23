package airband

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	"github.com/thatSFguy/swDefinedRadio/internal/audio"
	"github.com/thatSFguy/swDefinedRadio/internal/demod"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

//go:embed web
var webFS embed.FS

func fsSub() (fs.FS, error) { return fs.Sub(webFS, "web") }

// Handler builds the page and its API. ctx bounds the audio stream,
// which is the one endpoint that does not end on its own.
func (a *App) Handler(ctx context.Context) (http.Handler, error) {
	mux := http.NewServeMux()

	// Live audio as an endless WAV, which any browser will play from an
	// <audio> element with no client-side decoding.
	mux.HandleFunc("GET /audio.wav", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		if _, err := w.Write(audio.WAVHeader(demod.AudioRate, 1, 16)); err != nil {
			return
		}
		flusher.Flush()

		ch := a.audio.Subscribe()
		defer a.audio.Unsubscribe(ch)
		buf := make([]byte, 0, 8192)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				return
			case block, ok := <-ch:
				if !ok {
					return
				}
				buf = audio.LittleEndianPCM(buf[:0], block)
				if _, err := w.Write(buf); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		web.WriteJSON(w, a.State())
	})

	mux.HandleFunc("POST /api/tune", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FreqHz float64 `json:"freq_hz"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch err := a.Tune(uint32(req.FreqHz)); {
		case errors.Is(err, ErrNotOnAir):
			// Not the caller's mistake: the channel is remembered, the
			// radio is simply somewhere else.
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		web.WriteJSON(w, a.State())
	})

	mux.HandleFunc("POST /api/scan", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scanning bool `json:"scanning"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		a.SetScanning(req.Scanning)
		web.WriteJSON(w, a.State())
	})

	mux.HandleFunc("POST /api/squelch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Squelch float64 `json:"squelch"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		a.SetSquelch(req.Squelch)
		web.WriteJSON(w, a.State())
	})

	// A survey walks the whole 25 kHz grid with the receiver itself,
	// scoring which channels anything is ever heard on. It takes
	// minutes rather than seconds, and unlike a sweep it answers the
	// question actually being asked.
	mux.HandleFunc("POST /api/survey", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Running bool `json:"running"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Running {
			a.StartSurvey()
		} else {
			a.StopSurvey()
		}
		web.WriteJSON(w, a.State())
	})

	mux.HandleFunc("POST /api/survey/reset", func(w http.ResponseWriter, r *http.Request) {
		a.ResetSurvey()
		web.WriteJSON(w, a.State())
	})

	// What the survey heard, offered as candidates to listen to.
	mux.HandleFunc("POST /api/survey/offer", func(w http.ResponseWriter, r *http.Request) {
		found := a.OfferSurveyed()
		web.WriteJSON(w, map[string]any{"found": found, "state": a.State()})
	})

	mux.HandleFunc("GET /api/survey", func(w http.ResponseWriter, r *http.Request) {
		web.WriteJSON(w, map[string]any{"heard": a.SurveyResults()})
	})

	mux.HandleFunc("POST /api/channel/squelch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FreqHz  float64 `json:"freq_hz"`
			Squelch float64 `json:"squelch"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := a.SetChannelSquelch(uint32(req.FreqHz), req.Squelch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		web.WriteJSON(w, a.State())
	})

	mux.HandleFunc("POST /api/channels", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Channels []Channel `json:"channels"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := a.SetChannels(req.Channels); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		web.WriteJSON(w, a.State())
	})

	sub, err := fsSub()
	if err != nil {
		return nil, fmt.Errorf("web assets: %w", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return mux, nil
}
