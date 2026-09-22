package alert

// shellCommand runs an alert command through the Windows command
// interpreter. There is no sh, and requiring one would mean the alert
// command only worked for people who happened to have Git for Windows
// installed.
func shellCommand(command string) (string, []string) {
	return "cmd", []string{"/c", command}
}
