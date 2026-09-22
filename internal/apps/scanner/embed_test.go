package scanner

import (
	"io/fs"
	"testing"
)

// The page is compiled into the binary by a go:embed directive, and
// getting one of those wrong — a moved directory, a missed file — fails
// silently at build time and only shows up as a blank page. This notices.
func TestEmbeddedUI(t *testing.T) {
	sub, err := fsSub()
	if err != nil {
		t.Fatalf("web assets: %v", err)
	}
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("index.html is not embedded: %v", err)
	}
	if len(b) < 500 {
		t.Errorf("index.html is only %d bytes — that is not the page", len(b))
	}
}
