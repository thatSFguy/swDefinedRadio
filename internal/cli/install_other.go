//go:build !windows

package cli

import "fmt"

// Install exists off Windows only to explain that it is not needed.
//
// On Linux and macOS there is nothing for it to do that the system's own
// tools do not do better: the rtl-sdr programs come from the package
// manager, the dongle needs a udev rule rather than a driver swap, and
// putting a binary on the PATH is a symlink.
func Install([]string) int {
	fmt.Println("install is for Windows, where there is no package manager to do it.")
	fmt.Println()
	fmt.Println("Here, install what the receivers drive:")
	fmt.Println("    sudo apt install rtl-sdr rtl-433      # and sox, for FM on speakers")
	fmt.Println()
	fmt.Println("and put this binary somewhere on your PATH:")
	fmt.Println("    ln -s \"$PWD/sdr\" ~/.local/bin/sdr")
	fmt.Println()
	fmt.Println("If the dongle cannot be opened, it is a permissions problem rather")
	fmt.Println("than a driver one — see Permissions in the README.")
	return 0
}

// Uninstall likewise.
func Uninstall([]string) int {
	fmt.Println("uninstall is for Windows. Here, remove the symlink you made.")
	return 0
}

// OfferInstall is a Windows affair. Everywhere else the rtl-sdr programs
// come from the package manager, and a missing one is a sentence the
// package manager already says better than we could.
func OfferInstall() bool { return true }

// PauseAtExit likewise: nothing here is launched by double-clicking it
// into a console that vanishes.
func PauseAtExit() {}
