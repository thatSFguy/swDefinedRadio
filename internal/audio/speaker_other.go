//go:build !linux

package audio

import "os"

// speakerEnv leaves the environment alone. The WSL audio socket the Linux
// build points sox at means nothing here.
func speakerEnv() []string { return os.Environ() }
