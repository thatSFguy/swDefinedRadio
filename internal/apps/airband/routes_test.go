package airband

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The page and the server agree about the endpoints by nothing more than
// both having been edited correctly, and they have already disagreed
// once: an edit that replaced one group of handlers swallowed the two
// next to it, and the page went on calling them. Nothing failed to
// compile, no test failed, and the Keep button quietly did nothing.
//
// This asks the page what it calls and checks the server answers.
func TestEveryEndpointThePageCallsExists(t *testing.T) {
	a := testApp(t, Channel{Name: "Guard", Hz: Guard})
	h, err := a.Handler(context.Background())
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	page, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("reading the page: %v", err)
	}

	// Both the shapes the page uses to reach the server.
	calls := regexp.MustCompile(`(?:fetch|post)\('(/[a-z0-9/._-]+)'`)
	seen := map[string]bool{}
	for _, m := range calls.FindAllStringSubmatch(string(page), -1) {
		seen[m[1]] = true
	}
	// The page draws itself from its state poll; an edit that cut the
	// poll left every card empty and nothing else failed.
	if !seen["/api/state"] {
		t.Error("the page never fetches /api/state, so it will never draw")
	}
	if len(seen) < 5 {
		t.Fatalf("only found %d endpoints in the page; the pattern is wrong", len(seen))
	}

	for path := range seen {
		// A path ending in a slash is a prefix the page completes with a
		// name — deleting a recording — and there is no name to try here.
		if strings.HasSuffix(path, "/") {
			continue
		}
		// Everything the page posts to is a JSON endpoint; an empty body
		// is enough to prove the route is there, since a missing one
		// answers 404, or 405 where the file server matches the path but
		// not the method.
		req, err := http.NewRequest("POST", srv.URL+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusNotFound:
			t.Errorf("the page calls %s and nothing is registered for it", path)
		case http.StatusMethodNotAllowed:
			// GET-only endpoints are fine; the page fetches those.
			g, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			g.Body.Close()
			if g.StatusCode == http.StatusNotFound || g.StatusCode == http.StatusMethodNotAllowed {
				t.Errorf("the page calls %s and it answers %d to POST and %d to GET",
					path, resp.StatusCode, g.StatusCode)
			}
		}
	}
}
