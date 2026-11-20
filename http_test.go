package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// keysFile writes a keys file for the named holders and returns its path
// and each holder's key.
func keysFile(t *testing.T, names ...string) (path string, keys map[string]string) {
	t.Helper()
	keys = map[string]string{}
	var lines []string
	for _, name := range names {
		key, line, err := newKey(name)
		if err != nil {
			t.Fatal(err)
		}
		keys[name] = key
		lines = append(lines, line)
	}
	path = filepath.Join(t.TempDir(), "keys")
	content := "# test keys\n\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, keys
}

// hosted starts the HTTP handler the way -http does.
func hosted(t *testing.T, keysPath string, perMinute float64, burst int) *httptest.Server {
	t.Helper()
	keys, err := openKeyStore(keysPath)
	if err != nil {
		t.Fatal(err)
	}
	server := newServer(newCatalog(newSource("testdata/site")), 1<<20)
	ts := httptest.NewServer(newHTTPHandler(server, keys, newLimiters(perMinute, burst), 1<<20))
	t.Cleanup(ts.Close)
	return ts
}

const listTools = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// post sends one MCP request with the given Authorization header.
func post(t *testing.T, url, authorization string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(listTools))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHTTPNeedsAKey(t *testing.T) {
	path, keys := keysFile(t, "alice")
	ts := hosted(t, path, 600, 100)

	for name, authorization := range map[string]string{
		"no header":         "",
		"unknown key":       "Bearer ojs_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"not a bearer":      "Basic " + keys["alice"],
		"key without ojs_":  "Bearer " + strings.TrimPrefix(keys["alice"], keyPrefix),
		"the hash, not key": "Bearer " + hashKey(keys["alice"]),
	} {
		t.Run(name, func(t *testing.T) {
			resp := post(t, ts.URL, authorization)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("no WWW-Authenticate header")
			}
		})
	}

	if resp := post(t, ts.URL, "Bearer "+keys["alice"]); resp.StatusCode != http.StatusOK {
		t.Errorf("with a good key: status = %d, want 200", resp.StatusCode)
	}
}

func TestHealthzIsOpen(t *testing.T) {
	path, _ := keysFile(t, "alice")
	ts := hosted(t, path, 600, 100)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// Each key has its own budget: one holder running out does not slow
// another down.
func TestHTTPRateLimitIsPerKey(t *testing.T) {
	path, keys := keysFile(t, "alice", "bob")
	ts := hosted(t, path, 1, 3) // 3 at once, then one a minute

	for i := range 3 {
		if resp := post(t, ts.URL, "Bearer "+keys["alice"]); resp.StatusCode != http.StatusOK {
			t.Fatalf("alice's request %d: status = %d, want 200", i+1, resp.StatusCode)
		}
	}
	resp := post(t, ts.URL, "Bearer "+keys["alice"])
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("alice's 4th request: status = %d, want 429", resp.StatusCode)
	}
	if wait, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || wait < 1 || wait > 60 {
		t.Errorf("Retry-After = %q, want 1 to 60 seconds", resp.Header.Get("Retry-After"))
	}
	if resp := post(t, ts.URL, "Bearer "+keys["bob"]); resp.StatusCode != http.StatusOK {
		t.Errorf("bob, after alice ran out: status = %d, want 200", resp.StatusCode)
	}
	// Requests without a key don't use up anyone's budget.
	if resp := post(t, ts.URL, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", resp.StatusCode)
	}
}

func TestLimiterRefills(t *testing.T) {
	l := newLimiters(6000, 1) // one every 10ms
	if ok, _ := l.allow("alice"); !ok {
		t.Fatal("first request refused")
	}
	ok, wait := l.allow("alice")
	if ok || wait <= 0 || wait > 20*time.Millisecond {
		t.Fatalf("second request: ok = %v, wait = %v", ok, wait)
	}
	time.Sleep(wait + 5*time.Millisecond)
	if ok, _ := l.allow("alice"); !ok {
		t.Error("still refused after waiting")
	}
}

// Keys can be added and revoked by editing the file, with no restart.
func TestKeysFileIsReloaded(t *testing.T) {
	path, keys := keysFile(t, "alice")
	store, err := openKeyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	recheck := func() { // pretend the last check was a while ago
		store.mu.Lock()
		store.checked = time.Time{}
		store.mu.Unlock()
	}
	rewrite := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		// Make sure the change is visible even on a coarse file clock.
		later := time.Now().Add(time.Duration(len(content)) * time.Second)
		if err := os.Chtimes(path, later, later); err != nil {
			t.Fatal(err)
		}
		recheck()
	}

	if name, ok := store.holder(keys["alice"]); !ok || name != "alice" {
		t.Fatalf("alice's key: %q, %v", name, ok)
	}

	bobKey, bobLine, _ := newKey("bob")
	rewrite(bobLine + "\n")
	if name, ok := store.holder(bobKey); !ok || name != "bob" {
		t.Errorf("bob's new key: %q, %v", name, ok)
	}
	if _, ok := store.holder(keys["alice"]); ok {
		t.Error("alice's key still works after it was removed")
	}

	// A broken file must not lock out the keys that were working.
	rewrite("this is not a keys file\n")
	if _, ok := store.holder(bobKey); !ok {
		t.Error("a broken keys file revoked bob's key")
	}
}

func TestKeysFileErrors(t *testing.T) {
	_, line, _ := newKey("alice")
	for name, content := range map[string]string{
		"a raw key, not a hash": "alice ojs_notahash\n",
		"three fields":          line + " extra\n",
		"bad name":              "bad/name " + strings.Fields(line)[1] + "\n",
		"same name twice":       line + "\n" + line + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := openKeyStore(path); err == nil || !strings.Contains(err.Error(), "line") {
				t.Errorf("err = %v, want one naming the line", err)
			}
		})
	}
	if _, err := openKeyStore(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing keys file was accepted")
	}
}

func TestNewKey(t *testing.T) {
	key, line, err := newKey("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, keyPrefix) || len(key) != len(keyPrefix)+43 {
		t.Errorf("key = %q, want ojs_ and 43 characters", key)
	}
	if strings.Contains(line, key) {
		t.Error("the keys file line contains the key itself")
	}
	if line != "alice@example.com "+hashKey(key) {
		t.Errorf("line = %q", line)
	}
	other, _, _ := newKey("alice@example.com")
	if other == key {
		t.Error("two keys came out the same")
	}
	for _, bad := range []string{"", "has space", "a/b", strings.Repeat("x", 65)} {
		if _, _, err := newKey(bad); err == nil {
			t.Errorf("newKey(%q) was accepted", bad)
		}
	}
}

// bearer adds an API key to every request, as an agent configured with an
// Authorization header does.
type bearer struct{ key string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}

// A real MCP client can connect over HTTP with a key and use the tools.
func TestHTTPEndToEnd(t *testing.T) {
	path, keys := keysFile(t, "alice")
	ts := hosted(t, path, 600, 100)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{keys["alice"]}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	var rep Report
	call(t, session, "validate", map[string]any{"yaml": route + "  to: {}\n  hostname: x\n"}, &rep)
	if rep.Summary.Invalid != 1 || rep.Schemas != "v4.22-next-standalone-strict" {
		t.Errorf("report = %+v", rep)
	}
}

func TestCheckHealth(t *testing.T) {
	path, _ := keysFile(t, "alice")
	ts := hosted(t, path, 600, 100)
	if err := checkHealth(ts.URL + "/healthz"); err != nil {
		t.Errorf("a running server: %v", err)
	}
	if err := checkHealth(ts.URL + "/mcp"); err == nil {
		t.Error("a 401 counted as healthy")
	}
	ts.Close()
	if err := checkHealth(ts.URL + "/healthz"); err == nil {
		t.Error("a stopped server counted as healthy")
	}
}
