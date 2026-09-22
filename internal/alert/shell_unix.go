//go:build !windows

package alert

// shellCommand runs an alert command through the shell, so that the
// pipes, quoting and redirection someone would naturally write in one
// all behave as they expect.
func shellCommand(command string) (string, []string) {
	return "sh", []string{"-c", command}
}
