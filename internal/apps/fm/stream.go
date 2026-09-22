package fm

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

func handler(ctx context.Context, st *station, b *audio.Broadcaster) (http.Handler, error) {
	mux := http.NewServeMux()

	// Live audio as a WAV stream, which every browser can play from an
	// <audio> element without any client-side decoding.
	mux.HandleFunc("GET /audio.wav", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		if _, err := w.Write(audio.WAVHeader(demod.AudioRate, 1, 16)); err != nil {
			return
		}
		flusher.Flush()

		ch := b.Subscribe()
		defer b.Unsubscribe(ch)

		buf := make([]byte, 0, 16384)
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

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		freq, level := st.get()
		web.WriteJSON(w, map[string]any{
			"freq_hz":    freq,
			"level":      level,
			"listeners":  b.Count(),
			"audio_rate": demod.AudioRate,
			"band_low":   uint32(BandLowHz),
			"band_high":  uint32(BandHighHz),
		})
	})

	mux.HandleFunc("POST /api/tune", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FreqHz float64 `json:"freq_hz"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch err := st.tune(uint32(req.FreqHz)); {
		case errors.Is(err, ErrNotOnAir):
			// Not a bad request: the radio simply moved on between the
			// page being drawn and the dial being turned.
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		freq, level := st.get()
		web.WriteJSON(w, map[string]any{"freq_hz": freq, "level": level})
	})

	sub, err := fsSub()
	if err != nil {
		return nil, fmt.Errorf("web assets: %w", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	return mux, nil
}
