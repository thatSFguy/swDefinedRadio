package audio

import (
	"bufio"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

// maxData is the most sound a WAV file can describe: its sizes are 32
// bits. At 48 kHz that is over twelve hours, so a recording reaching it
// has been forgotten about rather than wanted.
const maxData = 0xFFFFFFFF - 36

// WAVFile is a recording being written to disk.
//
// Unlike the live stream, a file has an end, so the header's sizes are
// filled in when it is closed. Until then they say zero, which is also
// what a file left behind by a crash says; OpenStore puts those right.
type WAVFile struct {
	f *os.File
	w *bufio.Writer
	n int64 // bytes of sound written
}

// CreateWAV starts a mono 16-bit recording at AudioRate.
func CreateWAV(path string) (*WAVFile, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	h := WAVHeader(AudioRate, 1, 16)
	binary.LittleEndian.PutUint32(h[4:], 36)
	binary.LittleEndian.PutUint32(h[40:], 0)
	w := bufio.NewWriterSize(f, 64<<10)
	if _, err := w.Write(h); err != nil {
		f.Close()
		return nil, err
	}
	return &WAVFile{f: f, w: w}, nil
}

// Write appends a block of sound. It refuses once the file is full rather
// than writing a header that lies about the length.
func (r *WAVFile) Write(block []int16) error {
	if r.n+int64(2*len(block)) > maxData {
		return errors.New("recording has reached the most a WAV file can hold")
	}
	for _, s := range block {
		if err := r.w.WriteByte(byte(s)); err != nil {
			return err
		}
		if err := r.w.WriteByte(byte(s >> 8)); err != nil {
			return err
		}
	}
	r.n += int64(2 * len(block))
	return nil
}

// Seconds is how much sound has been written.
func (r *WAVFile) Seconds() float64 { return float64(r.n) / (2 * AudioRate) }

// Close finishes the file, writing the sizes into its header.
func (r *WAVFile) Close() error {
	err := r.w.Flush()
	if err == nil {
		err = patchSizes(r.f, r.n)
	}
	if cerr := r.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func patchSizes(f io.WriterAt, data int64) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(data+36))
	if _, err := f.WriteAt(b[:], 4); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(b[:], uint32(data))
	_, err := f.WriteAt(b[:], 40)
	return err
}

// Recording is a finished (or in-progress) file, as a page lists it.
//
// Name is the path within the store: "clip.wav" for a recording made on
// its own, or "session/clip.wav" for one made as part of a session.
type Recording struct {
	Name    string    `json:"name"`
	Session string    `json:"session,omitempty"`
	At      time.Time `json:"at"`
	Seconds float64   `json:"seconds"`
	Bytes   int64     `json:"bytes"`
	Active  bool      `json:"active,omitempty"` // still being written
}

// Store is a directory of recordings, and the one recording, if any,
// being written into it.
//
// It is written to from a receive loop, so Write never fails loudly: a
// full disk ends the recording and says so in the log, but it must not
// stop the radio.
type Store struct {
	dir string

	mu      sync.Mutex
	session string // directory new recordings go into; empty for the top
	cur     *WAVFile
	curName string
	curAt   time.Time
}

// OpenStore uses dir for recordings, creating it when the first one is
// made. Files left unfinished by a crash have their headers repaired, so
// what was recorded before it can still be played.
func OpenStore(dir string) *Store {
	s := &Store{dir: dir}
	names, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
	inSessions, _ := filepath.Glob(filepath.Join(dir, "*", "*.wav"))
	for _, p := range append(names, inSessions...) {
		if err := repair(p); err != nil {
			log.Printf("recordings: %s: %v", filepath.Base(p), err)
		}
	}
	return s
}

func repair(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() < 44 {
		return err
	}
	var b [4]byte
	if _, err := f.ReadAt(b[:], 40); err != nil {
		return err
	}
	data := min(fi.Size()-44, maxData)
	if int64(binary.LittleEndian.Uint32(b[:])) == data {
		return nil
	}
	return patchSizes(f, data)
}

// BeginSession gathers the recordings that follow into a directory of
// their own, so a sitting's worth of clips can be found, played and
// deleted together rather than picked out of everything else. The
// directory is made with the first recording, so a session in which
// nothing was heard leaves nothing behind.
func (s *Store) BeginSession(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := time.Now().Format("20060102-150405") + "_" + cleanLabel(label)
	s.session = base
	// Record released and pressed again within the second would otherwise
	// pour the new session into the old one.
	for i := 2; exists(filepath.Join(s.dir, s.session)); i++ {
		s.session = fmt.Sprintf("%s-%d", base, i)
	}
}

// EndSession sends later recordings back to the top of the store. A
// recording under way carries on where it is.
func (s *Store) EndSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = ""
}

// InSession says whether a session is open.
func (s *Store) InSession() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session != ""
}

// Start begins a new recording, labelled so the file says what it is,
// and returns its name within the store. Any recording already under way
// is finished first.
func (s *Store) Start(label string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	dir := filepath.Join(s.dir, s.session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	now := time.Now()
	base := now.Format("20060102-150405") + "_" + cleanLabel(label)
	file := base + ".wav"
	// Two clips in the same second on the same channel are rare but not
	// impossible on a busy frequency; the second must not replace the first.
	for i := 2; exists(filepath.Join(dir, file)); i++ {
		file = fmt.Sprintf("%s-%d.wav", base, i)
	}
	name := file
	if s.session != "" {
		name = s.session + "/" + file
	}
	w, err := CreateWAV(filepath.Join(s.dir, name))
	if err != nil {
		return "", err
	}
	s.cur, s.curName, s.curAt = w, name, now
	return name, nil
}

// Write adds to the recording under way, and does nothing when there is
// none, so a receive loop can call it unconditionally.
func (s *Store) Write(block []int16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return
	}
	if err := s.cur.Write(block); err != nil {
		log.Printf("recording %s stopped: %v", s.curName, err)
		s.stopLocked()
	}
}

// Stop finishes the recording under way, returning its name and length.
func (s *Store) Stop() (string, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopLocked()
}

func (s *Store) stopLocked() (string, float64) {
	if s.cur == nil {
		return "", 0
	}
	name, secs := s.curName, s.cur.Seconds()
	if err := s.cur.Close(); err != nil {
		log.Printf("recording %s: %v", name, err)
	}
	s.cur, s.curName = nil, ""
	return name, secs
}

// Current describes the recording under way, if there is one.
func (s *Store) Current() (Recording, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return Recording{}, false
	}
	return Recording{Name: s.curName, Session: sessionOf(s.curName), At: s.curAt,
		Seconds: s.cur.Seconds(), Bytes: 44 + s.cur.n, Active: true}, true
}

// Remove deletes a finished recording.
func (s *Store) Remove(name string) error {
	if !validName(name) {
		return os.ErrNotExist
	}
	s.mu.Lock()
	busy := s.cur != nil && s.curName == name
	s.mu.Unlock()
	if busy {
		return errors.New("that recording is still being made")
	}
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
		return err
	}
	// A session emptied of its last clip goes too, or the list fills with
	// directories that hold nothing. Remove refuses one that is not empty.
	if sess := sessionOf(name); sess != "" {
		_ = os.Remove(filepath.Join(s.dir, sess))
	}
	return nil
}

// List is every recording, newest first, sessions included.
func (s *Store) List() ([]Recording, error) {
	cur, _ := s.Current()
	var out []Recording
	var walk func(sub string) error
	walk = func(sub string) error {
		entries, err := os.ReadDir(filepath.Join(s.dir, sub))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, e := range entries {
			name := e.Name()
			if sub != "" {
				name = sub + "/" + name
			}
			if e.IsDir() {
				// One level only: sessions do not nest.
				if sub == "" && validSegment(e.Name()) {
					if err := walk(e.Name()); err != nil {
						return err
					}
				}
				continue
			}
			if !validName(name) {
				continue
			}
			if name == cur.Name {
				out = append(out, cur)
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, Recording{
				Name: name, Session: sub, At: fi.ModTime().Add(-secondsOf(fi.Size())),
				Seconds: secondsOf(fi.Size()).Seconds(), Bytes: fi.Size(),
			})
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	// Every file name starts with when it was made, so sorting on it
	// sorts by time whichever session it is in.
	slices.SortFunc(out, func(a, b Recording) int {
		return strings.Compare(path.Base(b.Name), path.Base(a.Name))
	})
	if out == nil {
		out = []Recording{}
	}
	return out, nil
}

//go:embed recordings.js
var recordingsJS []byte

// Routes adds the endpoints for listing, playing and deleting recordings,
// and the script a page uses to show them. Starting and stopping are
// left to each receiver, because what a recording is — one long take, or
// a clip per transmission — differs.
func (s *Store) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/recordings", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		web.WriteJSON(w, list)
	})

	// Served from disk with ranges, so a player can seek in it and the
	// browser can save it under its own name.
	mux.HandleFunc("GET /recordings/{name...}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !validName(name) {
			http.NotFound(w, r)
			return
		}
		if cur, ok := s.Current(); ok && cur.Name == name {
			http.Error(w, "that recording is still being made", http.StatusConflict)
			return
		}
		f, err := os.Open(filepath.Join(s.dir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		if r.URL.Query().Has("download") {
			w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(name)+`"`)
		}
		http.ServeContent(w, r, name, fi.ModTime(), f)
	})

	// Deleting is by the batch, because clips pile up by the hundred and
	// one request per clip is a page hammering the server. A name that
	// cannot be deleted — gone already, or still being written — does not
	// stop the rest.
	mux.HandleFunc("POST /api/recordings/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Names []string `json:"names"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		deleted, failed := 0, []string{}
		for _, name := range req.Names {
			if err := s.Remove(name); err != nil && !os.IsNotExist(err) {
				failed = append(failed, name)
				continue
			}
			deleted++
		}
		web.WriteJSON(w, map[string]any{"deleted": deleted, "failed": failed})
	})

	mux.HandleFunc("GET /recordings.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(recordingsJS)
	})
}

func secondsOf(size int64) time.Duration {
	if size < 44 {
		return 0
	}
	return time.Duration(float64(size-44) / (2 * AudioRate) * float64(time.Second))
}

// cleanLabel keeps a label to characters that are safe in a file name on
// any system the recordings might be copied to.
func cleanLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '_':
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_.")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	if out == "" {
		out = "recording"
	}
	return out
}

// validName accepts only names this package would have made — a file,
// or a file in a session — which is what keeps a request from reaching
// outside the directory.
func validName(name string) bool {
	sess, file, nested := strings.Cut(name, "/")
	if !nested {
		sess, file = "", name
	}
	if nested && !validSegment(sess) {
		return false
	}
	return strings.HasSuffix(file, ".wav") && validSegment(file)
}

// validSegment is one path element made only of the characters
// cleanLabel keeps, and never "." or "..".
func validSegment(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// sessionOf is the session a recording's name puts it in, if any.
func sessionOf(name string) string {
	if sess, _, ok := strings.Cut(name, "/"); ok {
		return sess
	}
	return ""
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
