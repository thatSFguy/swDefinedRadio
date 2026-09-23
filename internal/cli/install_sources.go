// Pinned upstream releases, and the checks that make downloading them
// safe enough to do on somebody's behalf.
//
// None of this is Windows-only in nature — only the install that uses it
// is — and keeping it here means the two things that actually protect a
// person can be tested on any machine.

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// The upstream projects. Their Windows builds are downloaded rather than
// carried in this repository: they are somebody else's work under their
// own licence, and pinning a copy here would mean shipping it stale.
//
// What is pinned instead is which release, and the SHA-256 of every file
// taken out of it. Asking GitHub for "the latest release" and running
// whatever comes back means the install is whatever upstream published
// this morning — not reproducible, and no way to notice if an account
// were taken over and a release replaced. These hashes were taken from
// the releases named below and are checked before anything is written.
const (
	rtlsdrRepo = "rtlsdrblog/rtl-sdr-blog"
	rtl433Repo = "merbanan/rtl_433"
)

// want names a file inside an archive, what to call it once installed,
// and the SHA-256 it must have.
type want struct{ from, to, sum string }

// source is one upstream archive: which release, which asset, and what
// is taken out of it.
type source struct {
	repo, tag, asset, match string
	wanted                  []want
}

var (
	// Everything else in this archive — import libraries, the other
	// rtl_* tools — is not needed to run this.
	rtlsdrSource = source{
		repo: rtlsdrRepo, tag: "V1.4.0", asset: "Release.zip", match: "Release.zip",
		wanted: []want{
			{"rtl_tcp.exe", "rtl_tcp.exe", "6ee31aa9db5d559eaaeaf69bb6ad874076be47e65dab407cd9b46c5ddc418881"},
			{"rtl_sdr.exe", "rtl_sdr.exe", "2897408e023a9c98897cc1c83f0f7c2ff595e38ab852243acc999852d14049c0"},
			{"rtl_test.exe", "rtl_test.exe", "3f986c700e338b30510edc2b99fad44a06f43770689301e1cfdc8f773b97f305"},
			{"rtlsdr.dll", "rtlsdr.dll", "0fa7f8bf4b7e07918a0a9e98228007cccac3109dbaac7497e116e416135d58a5"},
			{"msvcr100.dll", "msvcr100.dll", "2b7ab898b2f64f4cd1ae651c93d7eb9556208fd8d8d40d425e856c83d86d2346"},
			{"pthreadVC2.dll", "pthreadVC2.dll", "0e6af724609ef6846982ef717013426c359c455fff324e906d8d55c8bb88d16e"},
		},
	}

	// The statically linked build, installed under the name everything
	// looks for. The ordinary rtl_433.exe in the same archive wants
	// SoapySDR.dll and two others beside it and will not start without
	// them — one self-contained file is the better trade for a receiver
	// that only ever drives it as a child process.
	rtl433Source = source{
		repo: rtl433Repo, tag: "25.12", asset: "rtl_433-win-x64-25.12.zip", match: "win-x64",
		wanted: []want{
			{"rtl_433_64bit_static.exe", "rtl_433.exe", "af74548a95baa41239a143fee62c4b45c2adf08af571641845d56e28e6bc67a8"},
		},
	}
)

// sources is what the install downloads, and what it tells people it is
// about to download.
var sources = []source{rtlsdrSource, rtl433Source}

// Limits on what an install will swallow: a member of an archive, and a
// whole download.
const (
	maxMember   = 32 << 20
	maxDownload = 64 << 20
)

// github is where these downloads are allowed to come from. The asset
// URL in a -latest install is read out of an API response rather than
// written here, and a redirect can point anywhere at all, so both are
// checked rather than trusted.
func github(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" {
		return fmt.Errorf("refusing a %s download: %s", u.Scheme, raw)
	}
	switch u.Hostname() {
	case "github.com", "api.github.com",
		"objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return nil
	}
	return fmt.Errorf("refusing a download from %s", u.Hostname())
}

func wantedAs(name string, wanted []want) (want, bool) {
	for _, w := range wanted {
		if strings.EqualFold(name, w.from) {
			return w, true
		}
	}
	return want{}, false
}

// latestAsset asks GitHub for the current release and picks the asset
// whose name contains match. Asking rather than pinning a URL means an
// install done next year gets the release current then.
// describeSources says what will be downloaded, from where, and at which
// release. Anyone agreeing to this is agreeing to run somebody else's
// binaries, so it is worth saying so plainly rather than in a README
// they will not read.
func describeSources(indent string) {
	for _, src := range sources {
		names := make([]string, 0, len(src.wanted))
		for _, w := range src.wanted {
			names = append(names, w.to)
		}
		fmt.Printf("%shttps://github.com/%s  (release %s)\n", indent, src.repo, src.tag)
		fmt.Printf("%s  %s\n", indent, strings.Join(names, ", "))
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// verify is the gate every downloaded file passes before it is written.
// skip is -latest, where there is no recorded hash to compare against and
// the person has said so out loud.
func verify(name string, body []byte, sum string, skip bool) error {
	if skip {
		return nil
	}
	if got := sha256Hex(body); got != sum {
		return fmt.Errorf("%s does not match the release this was tested against\n"+
			"       expected sha256 %s\n       got      sha256 %s", name, sum, got)
	}
	return nil
}
