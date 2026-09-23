package audio

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A finished recording has to say how long it is, or a player that
// trusts the header plays nothing — and most of them trust it.
func TestWAVFileSizesAreWrittenOnClose(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.wav")
	w, err := CreateWAV(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(make([]int16, AudioRate)); err != nil {
		t.Fatal(err)
	}
	if got := w.Seconds(); got != 1 {
		t.Errorf("Seconds = %v, want 1", got)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 44+2*AudioRate {
		t.Fatalf("file is %d bytes, want %d", len(b), 44+2*AudioRate)
	}
	if got := binary.LittleEndian.Uint32(b[4:]); got != uint32(len(b)-8) {
		t.Errorf("RIFF size = %d, want %d", got, len(b)-8)
	}
	if got := binary.LittleEndian.Uint32(b[40:]); got != 2*AudioRate {
		t.Errorf("data size = %d, want %d", got, 2*AudioRate)
	}
}

// A recording cut short by a crash never had its header finished. What
// was written before it is still worth having, so opening the store puts
// the header right.
func TestOpenStoreRepairsAnUnfinishedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "20260101-000000_crash.wav")
	w, err := CreateWAV(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Write(make([]int16, 1000))
	_ = w.w.Flush()
	_ = w.f.Close() // no Close: the sizes are still zero

	OpenStore(dir)
	b, _ := os.ReadFile(p)
	if got := binary.LittleEndian.Uint32(b[40:]); got != 2000 {
		t.Errorf("data size after repair = %d, want 2000", got)
	}
}

func TestStoreRecordsListsAndRemoves(t *testing.T) {
	s := OpenStore(filepath.Join(t.TempDir(), "recordings"))
	s.Write(make([]int16, 100)) // nothing under way: must be harmless

	name, err := s.Start("98.7MHz")
	if err != nil {
		t.Fatal(err)
	}
	s.Write(make([]int16, AudioRate/2))
	if cur, ok := s.Current(); !ok || cur.Name != name || !cur.Active {
		t.Fatalf("Current = %+v, %v; want %s under way", cur, ok, name)
	}
	if err := s.Remove(name); err == nil {
		t.Error("a recording still being made was deleted")
	}
	if got, secs := s.Stop(); got != name || secs != 0.5 {
		t.Errorf("Stop = %s, %v; want %s, 0.5", got, secs, name)
	}

	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v; want one recording", list, err)
	}
	if list[0].Name != name || list[0].Seconds != 0.5 || list[0].Active {
		t.Errorf("listed %+v", list[0])
	}
	if err := s.Remove(name); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if list, _ := s.List(); len(list) != 0 {
		t.Errorf("still listed after removal: %v", list)
	}
}

// Two transmissions in the same second on the same channel must end up
// as two files, not one overwriting the other.
func TestStoreDoesNotOverwriteWithinASecond(t *testing.T) {
	s := OpenStore(t.TempDir())
	a, _ := s.Start("Guard")
	b, _ := s.Start("Guard")
	s.Stop()
	if a == b {
		t.Fatalf("both recordings were named %s", a)
	}
}

func TestCleanLabel(t *testing.T) {
	for in, want := range map[string]string{
		"121.500MHz Guard":              "121.500MHz_Guard",
		"122.800MHz Unicom — candidate": "122.800MHz_Unicom_candidate",
		"../../etc/passwd":              "etcpasswd",
		"":                              "recording",
	} {
		if got := cleanLabel(in); got != want {
			t.Errorf("cleanLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// The name in a request is the only thing standing between it and the
// rest of the disk.
func TestRoutesRefuseNamesOutsideTheStore(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o644)
	s := OpenStore(filepath.Join(dir, "recordings"))
	name, _ := s.Start("test")
	s.Write(make([]int16, 10))
	s.Stop()

	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, err := http.Get(srv.URL + "/recordings/" + name)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || len(body) != 64 || r.Header.Get("Content-Type") != "audio/wav" {
		t.Errorf("GET recording: %d, %d bytes, %s", r.StatusCode, len(body), r.Header.Get("Content-Type"))
	}

	for _, bad := range []string{"..%2Fsecret.txt", "secret.txt", ".hidden.wav"} {
		r, err := http.Get(srv.URL + "/recordings/" + bad)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode == 200 {
			t.Errorf("GET %s was served", bad)
		}
	}
}

// Clearing out a morning of clips is one request, and a name that cannot
// go — still being written — must not stop the others.
func TestBulkDelete(t *testing.T) {
	s := OpenStore(t.TempDir())
	var names []string
	for _, l := range []string{"a", "b", "c"} {
		n, _ := s.Start(l)
		names = append(names, n)
	}
	// c is still being written.

	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := `{"names":["` + names[0] + `","` + names[1] + `","` + names[2] + `","gone.wav"]}`
	r, err := http.Post(srv.URL+"/api/recordings/delete", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Deleted int      `json:"deleted"`
		Failed  []string `json:"failed"`
	}
	json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	// Already gone counts as deleted: the page asked for it not to exist.
	if got.Deleted != 3 || len(got.Failed) != 1 || got.Failed[0] != names[2] {
		t.Errorf("got %+v, want 3 deleted and %s refused", got, names[2])
	}
	list, _ := s.List()
	if len(list) != 1 || list[0].Name != names[2] {
		t.Errorf("left %v, want only the one being recorded", list)
	}
	s.Stop()
}

// Every page that records loads the list from here, so it has to be there.
func TestRecordingsScriptIsServed(t *testing.T) {
	mux := http.NewServeMux()
	OpenStore(t.TempDir()).Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	r, err := http.Get(srv.URL + "/recordings.js")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), "function Recordings(") {
		t.Errorf("GET /recordings.js: %d, %d bytes", r.StatusCode, len(b))
	}
}

// A session's recordings live in a directory of their own, are listed
// with it, can be fetched through it, and the directory goes when the
// last of them does.
func TestSessions(t *testing.T) {
	dir := t.TempDir()
	s := OpenStore(dir)
	loose, _ := s.Start("before")
	s.Stop()

	s.BeginSession("airband")
	a, _ := s.Start("121.500MHz Guard")
	s.Write(make([]int16, 480))
	b, _ := s.Start("121.500MHz Guard") // finishes a
	s.Stop()
	s.EndSession()

	sess, _, ok := strings.Cut(a, "/")
	if !ok || !strings.HasSuffix(sess, "_airband") || !strings.HasPrefix(b, sess+"/") {
		t.Fatalf("names %q, %q are not in one session", a, b)
	}
	list, _ := s.List()
	got := map[string]string{}
	for _, r := range list {
		got[r.Name] = r.Session
	}
	if len(list) != 3 || got[a] != sess || got[b] != sess || got[loose] != "" {
		t.Errorf("listed %+v", list)
	}

	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	r, err := http.Get(srv.URL + "/recordings/" + a)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Errorf("GET %s: %d", a, r.StatusCode)
	}

	for _, n := range []string{a, b} {
		if err := s.Remove(n); err != nil {
			t.Fatalf("Remove %s: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, sess)); !os.IsNotExist(err) {
		t.Errorf("the emptied session directory is still there")
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"20260101-000000_x.wav":   true,
		"20260101-000000_s/a.wav": true,
		"../x.wav":                false,
		"s/../x.wav":              false,
		"a/b/c.wav":               false,
		"/x.wav":                  false,
		"s/":                      false,
		"s/x.txt":                 false,
	} {
		if got := validName(name); got != want {
			t.Errorf("validName(%q) = %v, want %v", name, got, want)
		}
	}
}
