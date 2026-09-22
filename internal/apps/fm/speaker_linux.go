package fm

import "os"

// speakerEnv points sox at WSL's audio server.
//
// Under WSL there is no sound device of the usual kind; WSLg publishes a
// PulseAudio socket instead, and sox will not find it on its own. Setting
// this on a normal Linux desktop is harmless: PulseAudio is asked for a
// socket that is not there and sox falls back to what is.
func speakerEnv() []string {
	return append(os.Environ(), "PULSE_SERVER=unix:/mnt/wslg/PulseServer")
}
