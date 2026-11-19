package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// errNotFound means the schema source has no such file.
var errNotFound = errors.New("not found")

// A source reads files from a published schema tree: either the site
// (https://openshiftjsonschema.dev) or a local copy of its document root.
type source interface {
	read(ctx context.Context, path string) ([]byte, error)
}

func newSource(location string) source {
	if strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		return &httpSource{
			base:   strings.TrimRight(location, "/"),
			client: &http.Client{Timeout: 30 * time.Second},
		}
	}
	return dirSource{root: location}
}

type dirSource struct{ root string }

func (s dirSource) read(_ context.Context, path string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(path)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNotFound
	}
	return b, err
}

type httpSource struct {
	base   string
	client *http.Client
}

// Standalone schemas are a few MB at most; anything bigger is not one.
const maxSchemaBytes = 32 << 20

func (s *httpSource) read(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ojs-mcp/"+version)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GET %s: %s", req.URL, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxSchemaBytes))
}

// versionInfo is one entry of the site's versions.json.
type versionInfo struct {
	Minor      string            `json:"minor"`
	OpenShift  string            `json:"openshift"`
	Kubernetes string            `json:"kubernetes"`
	Dirs       map[string]string `json:"dirs"`
}

// flavour picks the schema directory to validate against. The next flavours
// validate more like the API server, so they win where they exist (4.18 on).
func (v versionInfo) flavour(strict bool) (name, dir string, ok bool) {
	names := []string{"next-standalone", "standalone"}
	if strict {
		names = []string{"next-standalone-strict", "standalone-strict"}
	}
	for _, n := range names {
		if d := v.Dirs[n]; d != "" {
			return n, d, true
		}
	}
	return "", "", false
}

// A catalog knows which versions are published and loads their schemas,
// keeping compiled ones in memory.
type catalog struct {
	src source

	mu       sync.Mutex
	versions []versionInfo
	loaded   time.Time
	schemas  map[string]*jsonschema.Schema // nil value: known to be missing
	indexes  map[string]*kindIndex         // by schema directory; nil value: it has none
}

const (
	versionsTTL = time.Hour
	maxCached   = 400
)

func newCatalog(src source) *catalog {
	return &catalog{
		src:     src,
		schemas: map[string]*jsonschema.Schema{},
		indexes: map[string]*kindIndex{},
	}
}

// list returns the published versions, oldest first. It re-reads
// versions.json now and then so a long-running server sees new minors.
func (c *catalog) list(ctx context.Context) ([]versionInfo, error) {
	c.mu.Lock()
	versions, fresh := c.versions, time.Since(c.loaded) < versionsTTL
	c.mu.Unlock()
	if versions != nil && fresh {
		return versions, nil
	}

	b, err := c.src.read(ctx, "versions.json")
	var doc struct {
		Versions []versionInfo `json:"versions"`
	}
	if err == nil {
		err = json.Unmarshal(b, &doc)
	}
	if err == nil && len(doc.Versions) == 0 {
		err = errors.New("it lists no versions")
	}
	if err != nil {
		if versions != nil {
			return versions, nil // keep serving the list we have
		}
		return nil, fmt.Errorf("reading versions.json: %w", err)
	}
	sort.SliceStable(doc.Versions, func(i, j int) bool {
		return minorLess(doc.Versions[i].Minor, doc.Versions[j].Minor)
	})

	c.mu.Lock()
	c.versions, c.loaded = doc.Versions, time.Now()
	c.schemas = map[string]*jsonschema.Schema{} // a minor may have been rebuilt
	c.indexes = map[string]*kindIndex{}
	c.mu.Unlock()
	return doc.Versions, nil
}

// resolve finds the published minor for a version such as "4.22", "v4.22" or
// "4.22.14". An empty version means the newest.
func (c *catalog) resolve(ctx context.Context, want string) (versionInfo, error) {
	versions, err := c.list(ctx)
	if err != nil {
		return versionInfo{}, err
	}
	want = strings.TrimPrefix(strings.TrimSpace(want), "v")
	if want == "" || want == "latest" {
		return versions[len(versions)-1], nil
	}
	parts := strings.Split(want, ".")
	if len(parts) >= 2 {
		minor := parts[0] + "." + parts[1]
		for _, v := range versions {
			if v.Minor == minor {
				return v, nil
			}
		}
	}
	var have []string
	for _, v := range versions {
		have = append(have, v.Minor)
	}
	return versionInfo{}, fmt.Errorf("no schemas for OpenShift %q; available versions: %s",
		want, strings.Join(have, ", "))
}

// schema returns the compiled schema at dir/file, or errNotFound.
func (c *catalog) schema(ctx context.Context, dir, file string) (*jsonschema.Schema, error) {
	key := dir + "/" + file
	c.mu.Lock()
	s, ok := c.schemas[key]
	c.mu.Unlock()
	if ok {
		if s == nil {
			return nil, errNotFound
		}
		return s, nil
	}

	b, err := c.src.read(ctx, key)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, err
	}
	if err == nil {
		if s, err = compile(key, b); err != nil {
			return nil, fmt.Errorf("schema %s: %w", key, err)
		}
	}

	c.mu.Lock()
	if len(c.schemas) >= maxCached {
		c.schemas = map[string]*jsonschema.Schema{}
	}
	c.schemas[key] = s
	c.mu.Unlock()
	if s == nil {
		return nil, errNotFound
	}
	return s, nil
}

// noLoader refuses to fetch anything: standalone schemas have every $ref
// inlined, so a schema that asks for another document is broken.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema refers to %s, which is not loaded", url)
}

func compile(name string, b []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	// Same settings as kubeconform, so results match what its users see.
	comp := jsonschema.NewCompiler()
	comp.DefaultDraft(jsonschema.Draft4)
	comp.UseLoader(noLoader{})
	url := "ojs:///" + name
	if err := comp.AddResource(url, doc); err != nil {
		return nil, err
	}
	return comp.Compile(url)
}

// minorLess orders "4.9" before "4.10".
func minorLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, _ := strconv.Atoi(pa[i])
		nb, _ := strconv.Atoi(pb[i])
		if na != nb {
			return na < nb
		}
	}
	return len(pa) < len(pb)
}
