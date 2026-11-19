package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect starts the server and returns a client session talking to it, the
// way an agent would.
func connect(t *testing.T, schemas string, maxInput int) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	server := newServer(newCatalog(newSource(schemas)), maxInput)
	if _, err := server.Connect(ctx, serverSide, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// call runs a tool and decodes its structured result into out.
func call(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if out != nil && !res.IsError {
		b, err := json.Marshal(res.StructuredContent)
		if err == nil {
			err = json.Unmarshal(b, out)
		}
		if err != nil {
			t.Fatalf("%s: reading the result: %v", tool, err)
		}
	}
	return res
}

func errorText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	text, _ := res.Content[0].(*mcp.TextContent)
	if text == nil {
		return ""
	}
	return text.Text
}

func TestToolsAreListed(t *testing.T) {
	s := connect(t, "testdata/site", 1<<20)
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}
	for _, name := range []string{"validate", "list_versions"} {
		tool := got[name]
		if tool == nil {
			t.Errorf("tool %s is missing", name)
			continue
		}
		if tool.Description == "" {
			t.Errorf("tool %s has no description", name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s is not marked read-only", name)
		}
	}
	if len(got) != 2 {
		t.Errorf("got %d tools, want 2", len(got))
	}
}

func TestListVersionsTool(t *testing.T) {
	s := connect(t, "testdata/site", 1<<20)
	var out VersionList
	call(t, s, "list_versions", nil, &out)
	if out.Latest != "4.22" {
		t.Errorf("latest = %q, want 4.22", out.Latest)
	}
	if len(out.Versions) != 2 || out.Versions[0].OpenShift != "4.9" || out.Versions[1].Kubernetes != "1.35" {
		t.Errorf("versions = %+v", out.Versions)
	}
}

func TestValidateTool(t *testing.T) {
	s := connect(t, "testdata/site", 1<<20)

	t.Run("strict by default", func(t *testing.T) {
		var rep Report
		call(t, s, "validate", map[string]any{"yaml": route + "  to: {}\n  hostname: x\n"}, &rep)
		if rep.Schemas != "v4.22-next-standalone-strict" || rep.Summary.Invalid != 1 {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("strict off", func(t *testing.T) {
		var rep Report
		call(t, s, "validate", map[string]any{
			"yaml": route + "  to: {}\n  hostname: x\n", "strict": false, "version": "v4.22.3",
		}, &rep)
		if rep.Schemas != "v4.22-next-standalone" || rep.Summary.Valid != 1 {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("unknown version is a tool error", func(t *testing.T) {
		res := call(t, s, "validate", map[string]any{"yaml": route, "version": "3.11"}, nil)
		if !res.IsError || !strings.Contains(errorText(res), "available versions") {
			t.Errorf("isError = %v, text = %q", res.IsError, errorText(res))
		}
	})
	t.Run("yaml is required", func(t *testing.T) {
		res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "validate", Arguments: map[string]any{}})
		if err == nil && !res.IsError {
			t.Error("a call without yaml was accepted")
		}
	})
}

func TestValidateToolInputLimit(t *testing.T) {
	s := connect(t, "testdata/site", 64)
	res := call(t, s, "validate", map[string]any{"yaml": route + strings.Repeat("# padding\n", 10)}, nil)
	if !res.IsError || !strings.Contains(errorText(res), "limit is 64") {
		t.Errorf("isError = %v, text = %q", res.IsError, errorText(res))
	}
}

// The default source is the website, so check the HTTP path too: schemas are
// fetched once and reused, and a 404 means "no schema for this kind".
func TestSchemasOverHTTP(t *testing.T) {
	hits := map[string]int{}
	files := http.FileServer(http.Dir("testdata/site"))
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		files.ServeHTTP(w, r)
	}))
	defer site.Close()

	s := connect(t, site.URL+"/", 1<<20)
	in := route + "  to: {}\n---\n" + route + "  to: {}\n---\napiVersion: example.com/v1\nkind: Widget\n"
	for range 2 {
		var rep Report
		call(t, s, "validate", map[string]any{"yaml": in}, &rep)
		if rep.Summary != (Summary{Valid: 2, Skipped: 1}) {
			t.Fatalf("summary = %+v, results = %+v", rep.Summary, rep.Results)
		}
	}
	for _, path := range []string{
		"/versions.json",
		"/v4.22-next-standalone-strict/route-route-v1.json",
		"/v4.22-next-standalone-strict/widget-example-v1.json",
	} {
		if hits[path] != 1 {
			t.Errorf("%s was fetched %d times, want 1", path, hits[path])
		}
	}
}

func TestBrokenSchemaSource(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer site.Close()

	s := connect(t, site.URL, 1<<20)
	res := call(t, s, "validate", map[string]any{"yaml": route}, nil)
	if !res.IsError || !strings.Contains(errorText(res), "versions.json") {
		t.Errorf("isError = %v, text = %q", res.IsError, errorText(res))
	}
}
