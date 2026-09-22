package scanner

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/scan"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

//go:embed web
var webFS embed.FS

func fsSub() (fs.FS, error) { return fs.Sub(webFS, "web") }

// sweepView is a sweep reduced for display: the same span, but with the
// power array decimated to something a browser can actually draw.
type sweepView struct {
	At    time.Time  `json:"at"`
	Start uint32     `json:"start"`
	Stop  uint32     `json:"stop"`
	BinHz float64    `json:"bin_hz"` // width of one decimated point
	Bins  int        `json:"bins"`   // bins in the underlying sweep
	Power scan.Power `json:"power"`
	Took  float64    `json:"took_s"`
}

// Handler builds the web UI and JSON API.
func handler(st *store, ctrl *control) (http.Handler, error) {
	mux := http.NewServeMux()

	// A sweep can run to hundreds of thousands of bins — far more than a
	// screen has pixels, and far too much to ship every couple of
	// seconds. Both endpoints decimate to the requested width, keeping
	// the strongest bin of each group so narrow carriers stay visible.
	mux.HandleFunc("GET /api/spectrum", func(w http.ResponseWriter, r *http.Request) {
		sweep, peaks, count := st.snapshot()

		var view *sweepView
		if sweep != nil {
			p := scan.Decimate(sweep.Power, widthParam(r, 1600))
			view = &sweepView{
				At:    sweep.At,
				Start: sweep.Start,
				Stop:  sweep.Stop,
				BinHz: float64(sweep.Stop-sweep.Start) / float64(len(p)),
				Bins:  len(sweep.Power),
				Power: p,
				Took:  sweep.Took,
			}
		}

		web.WriteJSON(w, struct {
			Now   time.Time   `json:"now"`
			Count int         `json:"sweeps"`
			Sweep *sweepView  `json:"sweep"`
			Peaks []scan.Peak `json:"peaks"`
			Floor float64     `json:"floor"`
		}{time.Now(), count, view, peaks, floorOf(sweep)})
	})

	mux.HandleFunc("GET /api/waterfall", func(w http.ResponseWriter, r *http.Request) {
		rows := st.waterfall()
		width := widthParam(r, 800)
		out := make([]scan.Power, len(rows))
		for i, s := range rows {
			out[i] = scan.Decimate(s.Power, width)
		}
		web.WriteJSON(w, struct {
			Rows []scan.Power `json:"rows"`
		}{out})
	})

	// The UI's controls: read the current sweep settings, and change them.
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		web.WriteJSON(w, configView(ctrl))
	})

	mux.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		cur, thresh, sep, _ := ctrl.snapshot()
		req := struct {
			StartHz   *float64 `json:"start_hz"`
			StopHz    *float64 `json:"stop_hz"`
			Threshold *float64 `json:"threshold"`
			SepHz     *float64 `json:"sep_hz"`
			DwellMs   *float64 `json:"dwell_ms"`
			SettleMs  *float64 `json:"settle_ms"`
		}{}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		next := cur
		if req.StartHz != nil {
			next.Start = uint32(*req.StartHz)
		}
		if req.StopHz != nil {
			next.Stop = uint32(*req.StopHz)
		}
		if req.DwellMs != nil {
			next.Dwell = time.Duration(*req.DwellMs) * time.Millisecond
		}
		if req.SettleMs != nil {
			next.Settle = time.Duration(*req.SettleMs) * time.Millisecond
		}
		if req.Threshold != nil {
			thresh = *req.Threshold
		}
		if req.SepHz != nil {
			sep = *req.SepHz
		}

		if err := ctrl.apply(next, thresh, sep); err != nil {
			// The message names what is wrong with the request, so send
			// it back for the UI to show rather than a bare status.
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		web.WriteJSON(w, configView(ctrl))
	})

	sub, err := fsSub()
	if err != nil {
		return nil, fmt.Errorf("web assets: %w", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	return mux, nil
}

// configView is the sweep settings as the UI sees them, including the
// derived numbers it would otherwise have to work out for itself.
func configView(ctrl *control) any {
	cfg, thresh, sep, version := ctrl.snapshot()
	return struct {
		StartHz   uint32  `json:"start_hz"`
		StopHz    uint32  `json:"stop_hz"`
		Threshold float64 `json:"threshold"`
		SepHz     float64 `json:"sep_hz"`
		DwellMs   float64 `json:"dwell_ms"`
		SettleMs  float64 `json:"settle_ms"`
		Steps     int     `json:"steps"`
		Bins      int     `json:"bins"`
		BinHz     float64 `json:"bin_hz"`
		EstimateS float64 `json:"estimate_s"`
		Version   int     `json:"version"`
		TunerLow  uint32  `json:"tuner_low_hz"`
		TunerHigh uint32  `json:"tuner_high_hz"`
	}{
		StartHz: cfg.Start, StopHz: cfg.Stop,
		Threshold: thresh, SepHz: sep,
		DwellMs:  float64(cfg.Dwell.Milliseconds()),
		SettleMs: float64(cfg.Settle.Milliseconds()),
		Steps:    cfg.Steps(), Bins: cfg.Bins(), BinHz: cfg.BinHz(),
		EstimateS: EstimateSweep(cfg).Seconds(),
		Version:   version,
		TunerLow:  ctrl.tunerLimits().Low, TunerHigh: ctrl.tunerLimits().High,
	}
}

// widthParam reads a display width from the query, clamped to something
// sane so a stray value cannot ask for an enormous response.
func widthParam(r *http.Request, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get("width"))
	if err != nil || v <= 0 {
		return def
	}
	return min(v, 4000)
}

func floorOf(s *scan.Sweep) float64 {
	if s == nil {
		return 0
	}
	return scan.NoiseFloor(s.Power)
}
