package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"
)

// API keys look like "ojs_" followed by 32 random bytes. The server never
// stores a key, only its SHA-256, so the keys file is no use to a thief.
// The keys are long and random, so a plain hash is enough: there is nothing
// to guess.
const keyPrefix = "ojs_"

var keyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// newKey makes a key for a named holder. It returns the key, to give to
// them, and the line to add to the keys file.
func newKey(name string) (key, line string, err error) {
	if !keyName.MatchString(name) {
		return "", "", fmt.Errorf("key name %q: use letters, digits, '.', '_', '@' and '-', at most 64 characters", name)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	key = keyPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return key, name + " " + hashKey(key), nil
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// A keyStore holds the keys file: one "name sha256" pair per line. It reads
// the file again when it changes, so adding or revoking a key needs no
// restart.
type keyStore struct {
	path string

	mu      sync.Mutex
	names   map[string]string // sha256 of the key -> holder's name
	modTime time.Time
	checked time.Time
}

const keysRecheck = 5 * time.Second

func openKeyStore(path string) (*keyStore, error) {
	s := &keyStore{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *keyStore) load() error {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	names := map[string]string{}
	seen := map[string]bool{}
	scan := bufio.NewScanner(f)
	for n := 1; scan.Scan(); n++ {
		line, _, _ := strings.Cut(scan.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		bad := func(why string) error { return fmt.Errorf("%s line %d: %s", s.path, n, why) }
		if len(fields) != 2 {
			return bad(`want "name sha256"`)
		}
		name, sum := fields[0], strings.ToLower(fields[1])
		if !keyName.MatchString(name) {
			return bad("bad key name")
		}
		if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
			return bad("the second field is not a SHA-256 hash; add keys with ojs-mcp -new-key")
		}
		if seen[name] {
			return bad("name " + name + " is used twice")
		}
		seen[name] = true
		names[sum] = name
	}
	if err := scan.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.names, s.modTime, s.checked = names, info.ModTime(), time.Now()
	s.mu.Unlock()
	return nil
}

// holder returns the name the key was issued to, or false.
func (s *keyStore) holder(key string) (string, bool) {
	s.mu.Lock()
	stale := time.Since(s.checked) > keysRecheck
	if stale {
		s.checked = time.Now()
	}
	modTime := s.modTime
	s.mu.Unlock()

	if stale {
		if info, err := os.Stat(s.path); err != nil || !info.ModTime().Equal(modTime) {
			// Keep the keys we have if the new file is missing or broken:
			// a typo while editing must not lock everyone out.
			if err := s.load(); err != nil {
				log.Printf("keeping the current API keys: %v", err)
			} else {
				log.Printf("reloaded API keys from %s", s.path)
			}
		}
	}

	if !strings.HasPrefix(key, keyPrefix) {
		return "", false
	}
	s.mu.Lock()
	name, ok := s.names[hashKey(key)]
	s.mu.Unlock()
	return name, ok
}

// limiters gives each key holder their own request budget.
type limiters struct {
	perMinute float64
	burst     int

	mu     sync.Mutex
	byName map[string]*rate.Limiter
}

func newLimiters(perMinute float64, burst int) *limiters {
	return &limiters{perMinute: perMinute, burst: burst, byName: map[string]*rate.Limiter{}}
}

// allow spends one request from the holder's budget. When there is none
// left it returns how long until there is.
func (l *limiters) allow(name string) (ok bool, wait time.Duration) {
	l.mu.Lock()
	lim := l.byName[name]
	if lim == nil {
		lim = rate.NewLimiter(rate.Limit(l.perMinute/60), l.burst)
		l.byName[name] = lim
	}
	l.mu.Unlock()

	r := lim.Reserve()
	if d := r.Delay(); d > 0 {
		r.Cancel()
		return false, d
	}
	return true, 0
}

// newHTTPHandler serves MCP at /mcp to holders of an API key, each within
// their own rate limit, and an open health check at /healthz.
func newHTTPHandler(server *mcp.Server, keys *keyStore, limits *limiters, maxInput int) http.Handler {
	// Stateless with plain JSON replies: every call stands alone, so there
	// are no sessions to keep and the server can sit behind a proxy or CDN.
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:           true,
			JSONResponse:        true,
			MaxRequestBodyBytes: int64(maxInput)*2 + 64<<10, // JSON escaping can grow the YAML
		},
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		name := "-"
		// One line per request. Never the body: it is someone's manifest.
		defer func() {
			log.Printf("%s %s key=%s status=%d %s", r.Method, r.URL.Path, name, rec.status,
				time.Since(start).Round(time.Millisecond))
		}()

		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		holder, known := keys.holder(strings.TrimSpace(key))
		if !ok || !known {
			rec.Header().Set("WWW-Authenticate", `Bearer realm="ojs-mcp"`)
			http.Error(rec, "an API key is required: send it as 'Authorization: Bearer <key>'", http.StatusUnauthorized)
			return
		}
		name = holder

		if ok, wait := limits.allow(holder); !ok {
			rec.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			http.Error(rec, "rate limit reached for this API key; try again shortly", http.StatusTooManyRequests)
			return
		}
		mcpHandler.ServeHTTP(rec, r)
	})
	return mux
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// serveHTTP runs the HTTP server until the context ends.
func serveHTTP(done <-chan struct{}, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-done
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// checkHealth asks a running server whether it is up. Container images have
// no curl to do this with.
func checkHealth(url string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}
