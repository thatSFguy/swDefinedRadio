package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unsafe"
)

// Windows binds an RTL2832U stick to its television driver, or to
// nothing at all, and neither lets rtl_tcp open it. What it needs is
// WinUSB, and Windows already carries that: winusb.inf is an inbox,
// Microsoft-signed driver. It simply does not match the dongle's
// hardware ID, so Plug and Play never picks it.
//
// Binding it anyway is what Device Manager does when you choose "Let me
// pick", then Universal Serial Bus devices, then WinUsb Device. This does
// the same through SetupAPI, for the one device whose USB ID says it is
// an RTL2832U, so nothing is signed here, no certificate is trusted, and
// no driver package is downloaded. It needs administrator rights, which
// Windows asks for.

// The dongle's usual identifiers. A stock RTL2832U stick reports 2838;
// some report 2832, and the ones with an E4000 report 2839. A composite
// one appears as a parent and its interfaces, and interface 0 is the
// radio — the other is an infrared receiver nobody here wants.
var dongleID = regexp.MustCompile(`(?i)^USB\\VID_0BDA&PID_283[289](&MI_(\d\d))?\\`)

// The class winusb.inf installs into: Universal Serial Bus devices.
const usbDeviceClass = "{88BAE032-5A81-49F0-BC3D-A4FF138216D6}"

type dongle struct {
	id      string // device instance ID
	name    string
	service string // "" when no driver is bound
}

func (d dongle) ready() bool {
	return strings.EqualFold(d.service, "WinUSB") || strings.EqualFold(d.service, "libusbK")
}

func (d dongle) describe() string {
	if d.service == "" {
		return "has no driver"
	}
	return "is on the " + d.service + " driver"
}

// Driver installs WinUSB for the dongle, asking for elevation if it has
// to.
func Driver(args []string) int {
	fs := flag.NewFlagSet("driver", flag.ExitOnError)
	var (
		elevated = fs.Bool("elevated", false, "internal: this is the elevated copy")
		logPath  = fs.String("log", "", "internal: where the elevated copy reports")
	)
	fs.Parse(args)

	if *elevated {
		// Straight out, rather than back through main: this copy owns a
		// hidden console, so PauseAtExit would wait for an Enter that
		// nobody can press, and the copy that asked would wait on it.
		os.Exit(driverElevated(*logPath))
	}

	d, err := findDongle()
	switch {
	case err != nil:
		fmt.Printf("  [!!] could not look for the dongle: %v\n", err)
		return 1
	case d == nil:
		fmt.Println("  [ ] no RTL-SDR found — plug it in and try again.")
		fmt.Println("      If it is attached to WSL, give it back: usbipd detach --busid <id>")
		return 1
	case d.ready():
		fmt.Printf("  [ok] %s %s\n", d.name, d.describe())
		return 0
	}
	if err := installWinUSB(); err != nil {
		fmt.Printf("  [!!] WinUSB driver: %v\n", err)
		return 1
	}
	return 0
}

// offerDriver checks the dongle and, if it cannot be opened, offers to
// fix that. It never stops what follows: with the wrong driver the radio
// will not start, but the person may have declined for good reason.
//
// quiet says nothing unless there is something to fix, which is how it
// runs before every receiver starts.
func offerDriver(dry, quiet bool) {
	d, err := findDongle()
	switch {
	case quiet && (err != nil || d == nil || d.ready()):
		return
	case err != nil:
		fmt.Println("  [ ] could not check the dongle's driver")
		return
	case d == nil:
		fmt.Println("  [ ] no RTL-SDR found — plug it in, then: sdr driver")
		return
	case d.ready():
		fmt.Printf("  [ok] %s %s\n", d.name, d.describe())
		return
	}

	fmt.Printf("  [!!] %s %s, so nothing can open it\n", d.name, d.describe())
	if dry {
		fmt.Println("  [ ] WinUSB driver")
		return
	}
	fmt.Println()
	fmt.Println("  It needs WinUSB, which Windows already has: the same driver you would")
	fmt.Println("  get from Device Manager by choosing Universal Serial Bus devices, then")
	fmt.Println("  WinUsb Device. Only this device changes:")
	fmt.Printf("      %s\n", d.id)
	fmt.Println("  Windows will ask for administrator rights.")
	fmt.Println()
	if !interactive() {
		fmt.Println("  Nothing is asking, so nothing was changed. To do it:  sdr driver")
		return
	}
	if !yes("  Install the WinUSB driver now? [Y/n]: ") {
		fmt.Println("  Left alone. When you want it:  sdr driver")
		return
	}
	if err := installWinUSB(); err != nil {
		fmt.Printf("  [!!] WinUSB driver: %v\n", err)
		fmt.Println("       Zadig (https://zadig.akeo.ie) does the same by hand.")
	}
	if quiet {
		fmt.Println()
	}
}

// installWinUSB binds the driver, from an elevated copy of this program
// if this one is not already elevated.
func installWinUSB() error {
	if isAdmin() {
		reboot, err := bindWinUSB()
		if err != nil {
			return err
		}
		reportBound(os.Stdout, reboot)
		return nil
	}

	// The elevated copy gets a console of its own, which closes the
	// moment it finishes, so it reports through a file instead.
	f, err := os.CreateTemp("", "sdr-driver-*.log")
	if err != nil {
		return err
	}
	log := f.Name()
	f.Close()
	defer os.Remove(log)

	self, err := os.Executable()
	if err != nil {
		return err
	}
	code, err := runElevated(self, `driver -elevated -log "`+log+`"`)
	if err != nil {
		return err
	}
	out, _ := os.ReadFile(log)
	fmt.Print(string(out))
	if code != 0 {
		return fmt.Errorf("the elevated install failed")
	}
	return nil
}

func driverElevated(logPath string) int {
	w := os.Stdout
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return 1
		}
		defer f.Close()
		w = f
	}
	reboot, err := bindWinUSB()
	if err != nil {
		fmt.Fprintf(w, "  [!!] WinUSB driver: %v\n", err)
		return 1
	}
	reportBound(w, reboot)
	return 0
}

func reportBound(w *os.File, reboot bool) {
	fmt.Fprintln(w, "  [ok] WinUSB driver installed")
	if reboot {
		fmt.Fprintln(w, "       Windows says it needs a restart before the dongle can be used.")
	}
}

var (
	setupapi = syscall.NewLazyDLL("setupapi.dll")
	newdev   = syscall.NewLazyDLL("newdev.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	procGetClassDevs      = setupapi.NewProc("SetupDiGetClassDevsW")
	procDestroyList       = setupapi.NewProc("SetupDiDestroyDeviceInfoList")
	procEnumDeviceInfo    = setupapi.NewProc("SetupDiEnumDeviceInfo")
	procGetInstanceID     = setupapi.NewProc("SetupDiGetDeviceInstanceIdW")
	procGetProperty       = setupapi.NewProc("SetupDiGetDeviceRegistryPropertyW")
	procSetProperty       = setupapi.NewProc("SetupDiSetDeviceRegistryPropertyW")
	procGetInstallParams  = setupapi.NewProc("SetupDiGetDeviceInstallParamsW")
	procSetInstallParams  = setupapi.NewProc("SetupDiSetDeviceInstallParamsW")
	procBuildDriverList   = setupapi.NewProc("SetupDiBuildDriverInfoList")
	procEnumDriverInfo    = setupapi.NewProc("SetupDiEnumDriverInfoW")
	procSetSelectedDriver = setupapi.NewProc("SetupDiSetSelectedDriverW")
	procDiInstallDevice   = newdev.NewProc("DiInstallDevice")
	procIsUserAnAdmin     = shell32.NewProc("IsUserAnAdmin")
	procShellExecuteEx    = shell32.NewProc("ShellExecuteExW")
)

const (
	digcfPresent    = 0x2
	digcfAllClasses = 0x4

	spdrpDeviceDesc   = 0x0
	spdrpService      = 0x4
	spdrpClassGUID    = 0x8
	spdrpFriendlyName = 0xC

	diEnumSingleINF        = 0x00010000
	diFlagsExAllowExcluded = 0x00000800
	spditClassDriver       = 1
	errorNoMoreItems       = 259
	invalidHandle          = ^uintptr(0)
	seeMaskNoCloseProcess  = 0x40
	swHide                 = 0
	errorCancelled         = 1223
	waitInfinite           = 0xFFFFFFFF
)

// These follow the SDK's layouts on 64-bit Windows, where SetupAPI uses
// natural alignment; Go lays a struct out the same way.

type devInfoData struct {
	size      uint32
	classGUID [16]byte
	devInst   uint32
	reserved  uintptr
}

type devInstallParams struct {
	size                 uint32
	flags                uint32
	flagsEx              uint32
	hwndParent           uintptr
	installMsgHandler    uintptr
	installMsgHandlerCtx uintptr
	fileQueue            uintptr
	classInstallReserved uintptr
	reserved             uint32
	driverPath           [260]uint16
}

type drvInfoData struct {
	size          uint32
	driverType    uint32
	reserved      uintptr
	description   [256]uint16
	mfgName       [256]uint16
	providerName  [256]uint16
	driverDate    [2]uint32
	driverVersion uint64
}

type shellExecuteInfo struct {
	size       uint32
	mask       uint32
	hwnd       uintptr
	verb       *uint16
	file       *uint16
	parameters *uint16
	directory  *uint16
	show       int32
	instApp    uintptr
	idList     uintptr
	class      *uint16
	hkeyClass  uintptr
	hotKey     uint32
	icon       uintptr
	process    syscall.Handle
}

// usbDevices opens the list of USB devices that are plugged in.
func usbDevices() (uintptr, error) {
	enum, _ := syscall.UTF16PtrFromString("USB")
	h, _, err := procGetClassDevs.Call(0, uintptr(unsafe.Pointer(enum)), 0, digcfPresent|digcfAllClasses)
	if h == invalidHandle {
		return 0, err
	}
	return h, nil
}

// findDongle returns the RTL-SDR's radio interface, or nil if none is
// plugged in.
func findDongle() (*dongle, error) {
	h, err := usbDevices()
	if err != nil {
		return nil, err
	}
	defer procDestroyList.Call(h)

	d, _, err := findIn(h)
	return d, err
}

// findIn picks the dongle out of a device list, returning its entry in
// that list too so it can be acted on.
func findIn(h uintptr) (*dongle, devInfoData, error) {
	var (
		best     *dongle
		bestData devInfoData
		bestRank = -1
	)
	for i := uintptr(0); ; i++ {
		data := devInfoData{size: uint32(unsafe.Sizeof(devInfoData{}))}
		ok, _, err := procEnumDeviceInfo.Call(h, i, uintptr(unsafe.Pointer(&data)))
		if ok == 0 {
			if errno, _ := err.(syscall.Errno); errno == errorNoMoreItems {
				break
			}
			return nil, devInfoData{}, err
		}
		id := instanceID(h, &data)
		m := dongleID.FindStringSubmatch(id)
		if m == nil {
			continue
		}
		// Interface 0 of a composite device, then a plain device; the
		// composite parent and its infrared interface are not the radio.
		rank := 0
		switch m[2] {
		case "00":
			rank = 2
		case "":
			rank = 1
		}
		if rank == 0 || rank <= bestRank {
			continue
		}
		name := property(h, &data, spdrpFriendlyName)
		if name == "" {
			name = property(h, &data, spdrpDeviceDesc)
		}
		best = &dongle{id: id, name: name, service: property(h, &data, spdrpService)}
		bestData, bestRank = data, rank
	}
	// A composite parent is bound to usbccgp, which is right for it and
	// says nothing about the radio; it only counts when it is all there is.
	if best != nil && bestRank == 1 && strings.EqualFold(best.service, "usbccgp") {
		return nil, devInfoData{}, nil
	}
	return best, bestData, nil
}

func instanceID(h uintptr, data *devInfoData) string {
	var buf [512]uint16
	ok, _, _ := procGetInstanceID.Call(h, uintptr(unsafe.Pointer(data)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:])
}

func property(h uintptr, data *devInfoData, prop uintptr) string {
	var buf [512]uint16
	ok, _, _ := procGetProperty.Call(h, uintptr(unsafe.Pointer(data)), prop, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), 0)
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:])
}

// bindWinUSB installs the inbox WinUSB driver on the dongle. It must run
// elevated.
func bindWinUSB() (reboot bool, err error) {
	h, err := usbDevices()
	if err != nil {
		return false, err
	}
	defer procDestroyList.Call(h)

	d, data, err := findIn(h)
	switch {
	case err != nil:
		return false, err
	case d == nil:
		return false, errors.New("no RTL-SDR is plugged in")
	case d.ready():
		return false, nil
	}

	// A dongle with no driver has no class either, and the driver list
	// below is drawn from the device's class. Put it in the one
	// winusb.inf installs into, as choosing it in Device Manager does.
	class, _ := syscall.UTF16FromString(usbDeviceClass)
	ok, _, e := procSetProperty.Call(h, uintptr(unsafe.Pointer(&data)), spdrpClassGUID,
		uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)*2))
	if ok == 0 {
		return false, fmt.Errorf("set device class: %w", e)
	}

	// Look in winusb.inf alone, and take its entries even though none
	// names this device's hardware ID.
	params := devInstallParams{size: uint32(unsafe.Sizeof(devInstallParams{}))}
	ok, _, e = procGetInstallParams.Call(h, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&params)))
	if ok == 0 {
		return false, fmt.Errorf("read install parameters: %w", e)
	}
	inf := filepath.Join(os.Getenv("SystemRoot"), "INF", "winusb.inf")
	if _, err := os.Stat(inf); err != nil {
		return false, fmt.Errorf("this Windows has no %s", inf)
	}
	path, _ := syscall.UTF16FromString(inf)
	copy(params.driverPath[:], path)
	params.flags |= diEnumSingleINF
	params.flagsEx |= diFlagsExAllowExcluded
	ok, _, e = procSetInstallParams.Call(h, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&params)))
	if ok == 0 {
		return false, fmt.Errorf("set install parameters: %w", e)
	}

	ok, _, e = procBuildDriverList.Call(h, uintptr(unsafe.Pointer(&data)), spditClassDriver)
	if ok == 0 {
		return false, fmt.Errorf("read winusb.inf: %w", e)
	}
	drv, err := winUSBEntry(h, &data)
	if err != nil {
		return false, err
	}
	ok, _, e = procSetSelectedDriver.Call(h, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(drv)))
	if ok == 0 {
		return false, fmt.Errorf("select WinUSB: %w", e)
	}

	var needReboot int32
	ok, _, e = procDiInstallDevice.Call(0, h, uintptr(unsafe.Pointer(&data)),
		uintptr(unsafe.Pointer(drv)), 0, uintptr(unsafe.Pointer(&needReboot)))
	if ok == 0 {
		return false, fmt.Errorf("install: %w", e)
	}
	return needReboot != 0, nil
}

// winUSBEntry finds the WinUsb Device entry among what winusb.inf
// offers. It is the only one today, but taking the first entry blindly
// would install whatever a future version of the file happens to list
// first.
func winUSBEntry(h uintptr, data *devInfoData) (*drvInfoData, error) {
	var seen []string
	for i := uintptr(0); ; i++ {
		drv := &drvInfoData{size: uint32(unsafe.Sizeof(drvInfoData{}))}
		ok, _, err := procEnumDriverInfo.Call(h, uintptr(unsafe.Pointer(data)), spditClassDriver, i,
			uintptr(unsafe.Pointer(drv)))
		if ok == 0 {
			if errno, _ := err.(syscall.Errno); errno == errorNoMoreItems {
				break
			}
			return nil, fmt.Errorf("list winusb.inf: %w", err)
		}
		desc := syscall.UTF16ToString(drv.description[:])
		if strings.Contains(strings.ToLower(desc), "winusb") {
			return drv, nil
		}
		seen = append(seen, desc)
	}
	return nil, fmt.Errorf("winusb.inf offers no WinUsb Device entry (found %q)", seen)
}

func isAdmin() bool {
	ok, _, _ := procIsUserAnAdmin.Call()
	return ok != 0
}

// runElevated starts a program through the UAC prompt and waits for it.
func runElevated(file, params string) (int, error) {
	verb, _ := syscall.UTF16PtrFromString("runas")
	f, _ := syscall.UTF16PtrFromString(file)
	p, _ := syscall.UTF16PtrFromString(params)
	info := shellExecuteInfo{
		size:       uint32(unsafe.Sizeof(shellExecuteInfo{})),
		mask:       seeMaskNoCloseProcess,
		verb:       verb,
		file:       f,
		parameters: p,
		show:       swHide,
	}
	ok, _, err := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		if errno, _ := err.(syscall.Errno); errno == errorCancelled {
			return 0, errors.New("administrator rights were refused, so nothing changed")
		}
		return 0, err
	}
	defer syscall.CloseHandle(info.process)
	if _, err := syscall.WaitForSingleObject(info.process, waitInfinite); err != nil {
		return 0, err
	}
	var code uint32
	if err := syscall.GetExitCodeProcess(info.process, &code); err != nil {
		return 0, err
	}
	return int(code), nil
}
