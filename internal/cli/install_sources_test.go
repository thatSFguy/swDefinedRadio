package cli

import (
	"strings"
	"testing"
)

// TestDownloadsAreRestrictedToGitHub guards the check that stands between
// an install and running a binary from wherever an API response, or a
// redirect, happened to point.
func TestDownloadsAreRestrictedToGitHub(t *testing.T) {
	ok := []string{
		"https://github.com/rtlsdrblog/rtl-sdr-blog/releases/download/V1.4.0/Release.zip",
		"https://api.github.com/repos/merbanan/rtl_433/releases/latest",
		"https://objects.githubusercontent.com/whatever",
		"https://release-assets.githubusercontent.com/whatever",
	}
	for _, u := range ok {
		if err := github(u); err != nil {
			t.Errorf("%s was refused: %v", u, err)
		}
	}

	// Each of these is a way an attacker reaches the same place: plain
	// HTTP a redirect could downgrade to, a lookalike host, a host that
	// merely ends in the right name, and credentials that make a URL read
	// as GitHub to a person skimming it.
	bad := []string{
		"http://github.com/x/y/releases/download/v1/a.zip",
		"https://github.com.evil.test/x/y",
		"https://evilgithub.com/x/y",
		"https://notgithub.com/x/y",
		"https://github.com@evil.test/x/y",
		"ftp://github.com/x/y",
		"https://raw.githubusercontent.com/x/y",
	}
	for _, u := range bad {
		if err := github(u); err == nil {
			t.Errorf("%s was allowed", u)
		}
	}
}

// TestEveryDownloadedFileIsPinned is the one that matters: these files
// are executed. A release with no recorded hash is one nobody checked.
func TestEveryDownloadedFileIsPinned(t *testing.T) {
	if len(sources) == 0 {
		t.Fatal("no sources")
	}
	for _, src := range sources {
		if src.tag == "" || strings.Contains(src.tag, "latest") {
			t.Errorf("%s is not pinned to a release: %q", src.repo, src.tag)
		}
		if len(src.wanted) == 0 {
			t.Errorf("%s takes nothing out of its archive", src.repo)
		}
		for _, w := range src.wanted {
			if len(w.sum) != 64 {
				t.Errorf("%s: %s has no sha256 (%q)", src.repo, w.to, w.sum)
			}
			if strings.ContainsAny(w.to, `/\`) || strings.Contains(w.to, "..") {
				t.Errorf("%s: %q is a path, not a name — it would escape the install directory", src.repo, w.to)
			}
		}
	}
}

// TestWantedAsCarriesTheHash catches a rename that silently drops the
// verification, which would fail open rather than closed.
func TestWantedAsCarriesTheHash(t *testing.T) {
	w, ok := wantedAs("RTL_TCP.EXE", rtlsdrSource.wanted)
	if !ok {
		t.Fatal("rtl_tcp.exe was not matched (the archive spells it lower case)")
	}
	if w.to != "rtl_tcp.exe" || len(w.sum) != 64 {
		t.Errorf("matched %+v, want a name and a hash", w)
	}
	if _, ok := wantedAs("something-else.exe", rtlsdrSource.wanted); ok {
		t.Error("an unlisted file was accepted")
	}
}

// TestATamperedFileIsRejected proves the check fails closed. A hash that
// is merely recorded and never compared is decoration.
func TestATamperedFileIsRejected(t *testing.T) {
	const real = "the real program"
	sum := sha256Hex([]byte(real))

	if err := verify("rtl_tcp.exe", []byte(real), sum, false); err != nil {
		t.Fatalf("the genuine file was rejected: %v", err)
	}
	err := verify("rtl_tcp.exe", []byte("the real program, plus a back door"), sum, false)
	if err == nil {
		t.Fatal("a file that does not match its hash was accepted")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("unhelpful error: %v", err)
	}
	// -latest is the deliberate way past it, and only that.
	if err := verify("rtl_tcp.exe", []byte("anything at all"), sum, true); err != nil {
		t.Errorf("-latest should skip the check: %v", err)
	}
}
