package cli

import (
	"archive/zip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

// The upstream projects. Their Windows builds are downloaded rather than
// carried in this repository: they are somebody else's work under their
// own licence, and pinning a copy here would mean shipping it stale.
const (
	rtlsdrRepo = "rtlsdrblog/rtl-sdr-blog"
	rtl433Repo = "merbanan/rtl_433"
)

// want names a file inside an archive and what to call it once installed.
type want struct{ from, to string }

// wanted lists what is taken out of each archive. Everything else in them
// — import libraries, the other rtl_* tools — is not needed to run this.
var (
	rtlsdrWanted = []want{
		{"rtl_tcp.exe", "rtl_tcp.exe"},
		{"rtl_sdr.exe", "rtl_sdr.exe"},
		{"rtl_test.exe", "rtl_test.exe"},
		{"rtlsdr.dll", "rtlsdr.dll"},
		{"msvcr100.dll", "msvcr100.dll"},
		{"pthreadVC2.dll", "pthreadVC2.dll"},
	}

	// The statically linked build, installed under the name everything
	// looks for. The ordinary rtl_433.exe in the same archive wants
	// SoapySDR.dll and two others beside it and will not start without
	// them — one self-contained file is the better trade for a receiver
	// that only ever drives it as a child process.
	rtl433Wanted = []want{
		{"rtl_433_64bit_static.exe", "rtl_433.exe"},
	}
)

// Install puts this program, and what it needs, where Windows can find it.
func Install(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	var (
		dir        = fs.String("dir", defaultInstallDir(), "where to install")
		noDownload = fs.Bool("no-download", false, "do not fetch the rtl-sdr programs")
		noPath     = fs.Bool("no-path", false, "do not touch your PATH")
		noShortcut = fs.Bool("no-shortcut", false, "do not make a Start Menu shortcut")
		dryRun     = fs.Bool("dry-run", false, "say what would happen, and change nothing")
	)
	fs.Parse(args)

	fmt.Printf("installing to %s\n", *dir)
	if *dryRun {
		fmt.Println("  (dry run — nothing will be changed)")
	}

	if err := step(*dryRun, "sdr.exe copied", func() error {
		return copySelf(*dir)
	}); err != nil {
		return 1
	}

	if !*noDownload {
		_ = step(*dryRun, "rtl_tcp.exe, rtl_sdr.exe, rtl_test.exe", func() error {
			return fetchInto(*dir, rtlsdrRepo, "Release.zip", rtlsdrWanted)
		})
		_ = step(*dryRun, "rtl_433.exe", func() error {
			return fetchInto(*dir, rtl433Repo, "win-x64", rtl433Wanted)
		})
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
func fetchInto(dir, repo, match string, wanted []want) error {
	url, err := latestAsset(repo, match)
	if err != nil {
		return err
	}
	body, err := get(url)
	if err != nil {
		return err
	}
	defer os.Remove(body)
	defer func() {}()

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
		as, ok := wantedAs(filepath.Base(f.Name), wanted)
		if !ok {
			continue
		}
		if err := extract(f, filepath.Join(dir, as)); err != nil {
			return err
		}
		found++
	}
	if found != len(wanted) {
		return fmt.Errorf("found %d of %d expected files in %s",
			found, len(wanted), filepath.Base(url))
	}
	return nil
}

// wantedAs reports whether a file in the archive is one being installed,
// and under what name.
func wantedAs(name string, wanted []want) (string, bool) {
	for _, w := range wanted {
		if strings.EqualFold(name, w.from) {
			return w.to, true
		}
	}
	return "", false
}

func extract(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

// latestAsset asks GitHub for the current release and picks the asset
// whose name contains match. Asking rather than pinning a URL means an
// install done next year gets the release current then.
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
	c := &http.Client{Timeout: 3 * time.Minute}
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
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
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
