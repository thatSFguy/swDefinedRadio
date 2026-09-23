package cli

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Installing on Windows means four things, and only the first is about
// this program. The other three are why it is worth having a command for
// at all: somewhere on the PATH to be, the rtl-sdr programs every
// receiver drives, and a dongle bound to a driver that lets them open it.
//
// Nothing here needs administrator rights. It installs under the user's
// own profile and edits the user's own PATH, so the only thing that will
// ask for elevation is the driver, which is Zadig's business rather than
// ours.

// Install puts this program, and what it needs, where Windows can find it.
func Install(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	var (
		dir        = fs.String("dir", defaultInstallDir(), "where to install")
		noDownload = fs.Bool("no-download", false, "do not fetch the rtl-sdr programs")
		latest     = fs.Bool("latest", false, "take the newest upstream release instead of the tested one (skips the hash check)")
		noPath     = fs.Bool("no-path", false, "do not touch your PATH")
		noShortcut = fs.Bool("no-shortcut", false, "do not make a Start Menu shortcut")
		dryRun     = fs.Bool("dry-run", false, "say what would happen, and change nothing")
	)
	fs.Parse(args)

	fmt.Printf("installing to %s\n", *dir)
	if *dryRun {
		fmt.Println("  (dry run — nothing will be changed)")
	}
	if !*noDownload {
		fmt.Println()
		fmt.Println("This downloads and runs programs from other projects, under their")
		fmt.Println("own licences (GPL-2.0), verified against recorded SHA-256 hashes:")
		fmt.Println()
		describeSources("  ")
		fmt.Println()
	}

	if err := step(*dryRun, "sdr.exe copied", func() error {
		return copySelf(*dir)
	}); err != nil {
		return 1
	}

	// A download that quietly failed used to leave an install that looked
	// finished and then could not start a receiver, complaining about a
	// program the person had never heard of. These are the whole point of
	// the command, so a failure here is a failure.
	var short bool
	if !*noDownload {
		if *latest {
			fmt.Println("  [!!] -latest: taking whatever upstream published most recently,")
			fmt.Println("       which cannot be checked against a known hash.")
		}
		for _, src := range sources {
			names := make([]string, 0, len(src.wanted))
			for _, w := range src.wanted {
				names = append(names, w.to)
			}
			what := fmt.Sprintf("%s from %s %s", strings.Join(names, ", "), src.repo, src.tag)
			if err := step(*dryRun, what, func() error {
				return fetchInto(*dir, src, *latest)
			}); err != nil {
				short = true
			}
		}
	}

	if !*noPath {
		_ = step(*dryRun, "added to your PATH", func() error { return addToPath(*dir) })
	}
	if !*noShortcut {
		_ = step(*dryRun, "Start Menu shortcut", func() error { return makeShortcut(*dir) })
	}

	fmt.Println()
	reportDriver()
	fmt.Println()
	if short {
		fmt.Println("The programs the receivers drive could not be downloaded, so they")
		fmt.Println("will not start. Check the connection and run sdr install again, or")
		fmt.Printf("put rtl_tcp.exe, rtl_sdr.exe and rtl_433.exe in %s yourself.\n", *dir)
		return 1
	}
	fmt.Println("Open a new terminal, then: sdr")
	fmt.Println("To undo all of this:       sdr uninstall")
	return 0
}

// Uninstall puts everything back.
func Uninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	var (
		dir    = fs.String("dir", defaultInstallDir(), "where it was installed")
		dryRun = fs.Bool("dry-run", false, "say what would happen, and change nothing")
	)
	fs.Parse(args)

	fmt.Printf("removing %s\n", *dir)
	_ = step(*dryRun, "taken off your PATH", func() error { return removeFromPath(*dir) })
	_ = step(*dryRun, "Start Menu shortcut removed", func() error {
		if err := os.Remove(shortcutPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil // already gone is the outcome that was wanted
	})

	if *dryRun {
		fmt.Println("  [ ] files removed")
		return 0
	}
	switch left, err := removeInstalled(*dir); {
	case err != nil:
		fmt.Printf("  [!!] files removed: %v\n", err)
		return 1
	case left != "":
		// Uninstalling by running the very program being uninstalled is
		// the ordinary way to do it, and Windows will not delete a
		// running executable. Everything else is gone; this is a note,
		// not a failure.
		fmt.Println("  [ok] files removed, apart from the one still running")
		fmt.Printf("\n  Close this, then delete:\n    %s\n", *dir)
	default:
		fmt.Println("  [ok] files removed")
	}
	return 0
}

// removeInstalled deletes the install directory, and reports the running
// executable's path if that is what stopped it going entirely.
func removeInstalled(dir string) (left string, err error) {
	self, _ := os.Executable()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if same, _ := sameFile(self, p); same {
			left = p
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return "", err
		}
	}
	if left != "" {
		return left, nil
	}
	return "", os.Remove(dir)
}

// step runs one part of the install and reports how it went, in a form
// that can be read down the left-hand side.
func step(dry bool, what string, do func() error) error {
	if dry {
		fmt.Printf("  [ ] %s\n", what)
		return nil
	}
	if err := do(); err != nil {
		fmt.Printf("  [!!] %s: %v\n", what, err)
		return err
	}
	fmt.Printf("  [ok] %s\n", what)
	return nil
}

func defaultInstallDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "Programs", "sdr")
	}
	return filepath.Join(os.Getenv("USERPROFILE"), "sdr")
}

func shortcutPath() string {
	return filepath.Join(os.Getenv("APPDATA"),
		"Microsoft", "Windows", "Start Menu", "Programs", "sdr.lnk")
}

// copySelf copies the running executable into the install directory,
// which is how a program someone downloaded to their Downloads folder
// ends up somewhere sensible.
func copySelf(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, "sdr.exe")
	if same, _ := sameFile(self, dst); same {
		return nil // already installed, and being run from there
	}

	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()

	// Windows will not overwrite a running executable, so an upgrade
	// moves the old one aside first. The leftover is removed on the next
	// install, once nothing is running from it.
	if _, err := os.Stat(dst); err == nil {
		old := dst + ".old"
		_ = os.Remove(old)
		_ = os.Rename(dst, old)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// fetchInto downloads the latest release asset whose name contains match,
// and extracts the wanted files into dir. Paths inside the archive are
// ignored: the files are wanted beside the program, not in whatever
// folder structure the archive happens to use.
func fetchInto(dir string, src source, latest bool) error {
	url := "https://github.com/" + src.repo + "/releases/download/" + src.tag + "/" + src.asset
	if latest {
		var err error
		if url, err = latestAsset(src.repo, src.match); err != nil {
			return err
		}
	}
	body, err := get(url)
	if err != nil {
		return err
	}
	defer os.Remove(body)

	z, err := zip.OpenReader(body)
	if err != nil {
		return err
	}
	defer z.Close()

	found := 0
	for _, f := range z.File {
		// An x86 copy of the same name would otherwise overwrite the x64
		// one, depending on the order the archive happens to list them.
		if strings.Contains(f.Name, "x86/") {
			continue
		}
		w, ok := wantedAs(filepath.Base(f.Name), src.wanted)
		if !ok {
			continue
		}
		// The destination name comes from the list above, never from the
		// archive, so a member called ..\..\windows\system32\x.dll
		// cannot escape the install directory. Keep it that way.
		if err := extract(f, filepath.Join(dir, w.to), w.sum, latest); err != nil {
			return err
		}
		found++
	}
	if found != len(src.wanted) {
		return fmt.Errorf("found %d of %d expected files in %s",
			found, len(src.wanted), filepath.Base(url))
	}
	return nil
}

// extract writes one file out, but only once it is the file expected.
// Hashing what was downloaded before it is written is the whole defence
// against a replaced release: these are programs that will be run.
func extract(f *zip.File, dst, sum string, skipVerify bool) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	// A few megabytes each, and the limit is what stops a hostile or
	// broken archive from filling the disk through a decompression bomb.
	buf, err := io.ReadAll(io.LimitReader(rc, maxMember))
	if err != nil {
		return err
	}
	if int64(len(buf)) == maxMember {
		return fmt.Errorf("%s is larger than %d bytes", f.Name, maxMember)
	}

	if err := verify(f.Name, buf, sum, skipVerify); err != nil {
		return err
	}
	return os.WriteFile(dst, buf, 0o644)
}

func reportDriver() {
	out, err := powershell(driverScript)
	switch {
	case err != nil:
		fmt.Println("  [ ] could not check the dongle's driver")
		return
	case out == "absent":
		fmt.Println("  [ ] no RTL-SDR found — plug it in, then: sdr")
		return
	}
	service, name, _ := strings.Cut(out, "|")
	if strings.EqualFold(service, "WinUSB") || strings.EqualFold(service, "libusbK") {
		fmt.Printf("  [ok] %s is on the %s driver\n", name, service)
		return
	}
	fmt.Printf("  [!!] %s is on the %s driver, which will not let sdr open it\n", name, service)
	fmt.Println("       Install Zadig from https://zadig.akeo.ie, choose this device,")
	fmt.Println("       pick WinUSB, and press Replace Driver. Then run sdr again.")
}

// Nobody reads a switch before double-clicking. The first thing anyone
// does with a downloaded exe is run it, so running it is what has to
// offer the setup — otherwise the first thing they see is a complaint
// about rtl_tcp, which names a program they have never heard of and
// does not say what to do about it.

// OfferInstall asks whether to set things up, when it is plain that
// nobody has. It reports whether the command that follows can run.
func OfferInstall() bool {
	if installedHere() || haveRTLTCP() {
		return true
	}

	dir := defaultInstallDir()
	fmt.Println("sdr has not been set up on this computer yet.")
	fmt.Println()
	fmt.Println("The receivers do not talk to the dongle themselves. They drive")
	fmt.Println("programs from two other projects, and none of them are here.")
	fmt.Println("Setting up will, under your own account and without administrator")
	fmt.Println("rights:")
	fmt.Println()
	fmt.Printf("  - copy sdr.exe to %s\n", dir)
	fmt.Println("  - put that folder on your PATH, with a Start Menu shortcut")
	fmt.Println("  - download and run programs written by other people:")
	fmt.Println()
	describeSources("      ")
	fmt.Println()
	fmt.Println("    These are downloaded from GitHub over HTTPS, and each file is")
	fmt.Println("    checked against a SHA-256 recorded in this program before it is")
	fmt.Println("    written. They are not part of this project and carry their own")
	fmt.Println("    licences (GPL-2.0), which travel with the files.")
	fmt.Println()

	if !interactive() {
		fmt.Println("Nothing is asking, so nothing was changed. To set up:  sdr install")
		return false
	}
	if !yes("Set up now? [Y/n]: ") {
		fmt.Println()
		fmt.Println("Left alone. When you want it:  sdr install")
		return false
	}
	fmt.Println()

	if Install(nil) != 0 {
		return false
	}
	// The PATH edit reaches new terminals, not this one, and this one is
	// about to go looking for rtl_tcp.
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fmt.Println()
	fmt.Println("Carrying on with what you asked for.")
	fmt.Println()
	return true
}

// installedHere reports whether the running executable is the installed
// one, rather than a copy somebody downloaded and ran where it landed.
func installedHere() bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	same, _ := sameFile(self, filepath.Join(defaultInstallDir(), "sdr.exe"))
	return same
}

// haveRTLTCP reports whether the one program the hub cannot start
// without can be found — on the PATH, or sitting beside this binary,
// which is how a portable copy is arranged.
func haveRTLTCP() bool {
	if _, err := exec.LookPath("rtl_tcp"); err == nil {
		return true
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(filepath.Dir(self), "rtl_tcp.exe"))
	return err == nil
}

// interactive reports whether there is somebody at the other end to
// answer. Piped into a script, the honest thing is to say what would
// have been asked and change nothing.
func interactive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func yes(prompt string) bool {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// PauseAtExit keeps the window up when Explorer opened it, because a
// console it owns closes the moment the program returns and takes every
// word of the explanation with it.
func PauseAtExit() {
	if !ownConsole() || !interactive() {
		return
	}
	fmt.Print("\nPress Enter to close this window. ")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// ownConsole reports whether this process is the only one attached to
// its console, which is what being launched from Explorer looks like.
// Started from a terminal, the shell is on the console too.
func ownConsole() bool {
	var list [8]uint32
	n, _, _ := syscall.NewLazyDLL("kernel32.dll").
		NewProc("GetConsoleProcessList").
		Call(uintptr(unsafe.Pointer(&list[0])), uintptr(len(list)))
	return n == 1
}

func latestAsset(repo, match string) (string, error) {
	body, err := get("https://api.github.com/repos/" + repo + "/releases/latest")
	if err != nil {
		return "", err
	}
	defer os.Remove(body)

	f, err := os.Open(body)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(f).Decode(&rel); err != nil {
		return "", err
	}
	for _, a := range rel.Assets {
		if strings.Contains(a.Name, match) {
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("no %s build in %s %s", match, repo, rel.Tag)
}

// get downloads to a temporary file and returns its path, so that an
// archive of a few megabytes is not held in memory.
func get(url string) (string, error) {
	if err := github(url); err != nil {
		return "", err
	}
	c := &http.Client{
		Timeout: 3 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return github(req.URL.String())
		},
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "swdefinedradio-install")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp("", "sdr-download-*")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxDownload))
	if err != nil || n == maxDownload {
		os.Remove(tmp.Name())
		if err == nil {
			err = fmt.Errorf("%s is larger than %d bytes", url, maxDownload)
		}
		return "", err
	}
	return tmp.Name(), nil
}

// powershell runs a script with values passed in the environment, which
// avoids quoting anything into the command line.
func powershell(script string, env ...string) (string, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

const addPathScript = `
$d = $env:SDR_DIR
$p = [Environment]::GetEnvironmentVariable('Path','User')
if ($p -split ';' -contains $d) { Write-Output 'already'; exit }
if ([string]::IsNullOrEmpty($p)) { $new = $d } else { $new = $p.TrimEnd(';') + ';' + $d }
[Environment]::SetEnvironmentVariable('Path', $new, 'User')
Write-Output 'added'
`

func addToPath(dir string) error {
	_, err := powershell(addPathScript, "SDR_DIR="+dir)
	return err
}

const removePathScript = `
$d = $env:SDR_DIR
$p = [Environment]::GetEnvironmentVariable('Path','User')
if ([string]::IsNullOrEmpty($p)) { exit }
$kept = ($p -split ';' | Where-Object { $_ -ne $d -and $_ -ne '' }) -join ';'
[Environment]::SetEnvironmentVariable('Path', $kept, 'User')
`

func removeFromPath(dir string) error {
	_, err := powershell(removePathScript, "SDR_DIR="+dir)
	return err
}

const shortcutScript = `
$s = (New-Object -ComObject WScript.Shell).CreateShortcut($env:SDR_LNK)
$s.TargetPath = $env:SDR_EXE
$s.WorkingDirectory = $env:SDR_DIR
$s.Description = 'Software-defined radio receivers'
$s.Save()
`

func makeShortcut(dir string) error {
	lnk := shortcutPath()
	if err := os.MkdirAll(filepath.Dir(lnk), 0o755); err != nil {
		return err
	}
	_, err := powershell(shortcutScript,
		"SDR_LNK="+lnk, "SDR_EXE="+filepath.Join(dir, "sdr.exe"), "SDR_DIR="+dir)
	return err
}

// The dongle's usual identifiers. A stock RTL2832U stick reports 2838;
// some report 2832, and the ones with an E4000 report 2839.
const driverScript = `
$d = Get-PnpDevice -PresentOnly -ErrorAction SilentlyContinue |
     Where-Object { $_.InstanceId -match 'VID_0BDA&PID_283[289]' } |
     Select-Object -First 1
if ($null -eq $d) { Write-Output 'absent'; exit }
Write-Output ($d.Service + '|' + $d.FriendlyName)
`

// reportDriver says whether the dongle is bound to a driver these
// programs can open.
//
// This is the part an installer cannot do for you. Windows binds an
// RTL2832U stick to its television driver, which will not let anything
// else open it; Zadig rebinds it to WinUSB, and that needs administrator
// rights and a choice only a person should make, because the same dialog
// can unbind quite different hardware.
