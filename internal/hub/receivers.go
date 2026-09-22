package hub

import (
	"context"
	"net/http"

	"github.com/thatSFguy/swDefinedRadio/internal/apps/adsb"
	"github.com/thatSFguy/swDefinedRadio/internal/apps/fm"
	"github.com/thatSFguy/swDefinedRadio/internal/apps/scanner"
	"github.com/thatSFguy/swDefinedRadio/internal/apps/tpms"
	"github.com/thatSFguy/swDefinedRadio/internal/apps/uat"
	"github.com/thatSFguy/swDefinedRadio/internal/radio"
)

// The receivers differ in shape — one takes no sample stream at all, one
// retunes constantly, one wants its own subprocess — so each gets a small
// adapter rather than the five being bent into a common form. The
// adapters are where those differences are reconciled, and they are the
// only place that knows about more than one receiver.

// --- 1090 MHz aircraft ---------------------------------------------------

type adsbTab struct{ app *adsb.App }

func (t adsbTab) Meta() Meta { return Meta{"adsb", "Aircraft", "1090 MHz"} }

func (t adsbTab) Need() radio.Need {
	return radio.Need{Mode: radio.Samples, Tune: t.app.Radio()}
}

func (t adsbTab) Handler(context.Context) (http.Handler, error) { return t.app.Handler() }

func (t adsbTab) Run(ctx context.Context, h radio.Handle) error {
	go t.app.Status(ctx) // only while on the air; an idle receiver has nothing to say
	return t.app.Run(ctx, h.Stream)
}

// Background expires the aircraft table whether or not this receiver has
// the radio: an aircraft last heard four minutes ago has gone regardless.
func (t adsbTab) Background(ctx context.Context) { t.app.Expire(ctx) }

// --- 978 MHz aircraft ----------------------------------------------------

type uatTab struct{ app *uat.App }

func (t uatTab) Meta() Meta { return Meta{"uat", "UAT", "978 MHz"} }

func (t uatTab) Need() radio.Need {
	return radio.Need{Mode: radio.Samples, Tune: t.app.Radio()}
}

func (t uatTab) Handler(context.Context) (http.Handler, error) { return t.app.Handler() }

func (t uatTab) Run(ctx context.Context, h radio.Handle) error {
	go t.app.Status(ctx)
	return t.app.Run(ctx, h.Stream)
}

func (t uatTab) Background(ctx context.Context) { t.app.Expire(ctx) }

// --- tyre sensors --------------------------------------------------------

type tpmsTab struct {
	app  *tpms.App
	mode radio.Mode
}

func (t *tpmsTab) Meta() Meta { return Meta{"tpms", "Tyres", "315 / 433 MHz"} }

// Need asks for an address rather than samples: rtl_433 knows the sensor
// protocols and opens its own connection to the radio, so what this
// receiver wants is to be told where to find it. Exclusive is the
// fallback for hardware where that does not work, and hands rtl_433 the
// USB device outright.
func (t *tpmsTab) Need() radio.Need { return radio.Need{Mode: t.mode} }

func (t *tpmsTab) Handler(context.Context) (http.Handler, error) { return t.app.Handler() }

func (t *tpmsTab) Run(ctx context.Context, h radio.Handle) error {
	return t.app.RunAt(ctx, h.Addr)
}

// Background saves the sensor table periodically, which matters most
// while this receiver is off the air and nothing else would.
func (t *tpmsTab) Background(ctx context.Context) { t.app.Autosave(ctx) }

// --- spectrum ------------------------------------------------------------

type scannerTab struct{ app *scanner.App }

func (t scannerTab) Meta() Meta { return Meta{"scanner", "Spectrum", "sweep"} }

func (t scannerTab) Need() radio.Need {
	return radio.Need{Mode: radio.Samples, Tune: t.app.Radio()}
}

func (t scannerTab) Handler(context.Context) (http.Handler, error) { return t.app.Handler() }

func (t scannerTab) Run(ctx context.Context, h radio.Handle) error {
	defer t.app.DropHistory() // a waterfall is megabytes a row; see DropHistory
	return t.app.Run(ctx, h.Stream)
}

// --- broadcast FM --------------------------------------------------------

type fmTab struct{ app *fm.App }

func (t fmTab) Meta() Meta { return Meta{"fm", "FM", "87.5–108 MHz"} }

func (t fmTab) Need() radio.Need {
	return radio.Need{Mode: radio.Samples, Tune: t.app.Radio()}
}

func (t fmTab) Handler(ctx context.Context) (http.Handler, error) { return t.app.Handler(ctx) }

func (t fmTab) Run(ctx context.Context, h radio.Handle) error {
	return t.app.Run(ctx, h.Stream)
}

// compile-time checks that every adapter is a tab, and that the ones with
// work to do off the air are recognised as having it.
var (
	_ App = adsbTab{}
	_ App = uatTab{}
	_ App = (*tpmsTab)(nil)
	_ App = scannerTab{}
	_ App = fmTab{}

	_ Background = adsbTab{}
	_ Background = uatTab{}
	_ Background = (*tpmsTab)(nil)
)

// Receivers wraps built receivers as tabs, in the order they appear.
func Receivers(a *adsb.App, u *uat.App, t *tpms.App, tpmsMode radio.Mode,
	s *scanner.App, f *fm.App) []App {
	return []App{
		adsbTab{a}, uatTab{u}, &tpmsTab{t, tpmsMode}, scannerTab{s}, fmTab{f},
	}
}
