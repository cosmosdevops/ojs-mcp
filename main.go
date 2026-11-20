// ojs-mcp is an MCP server that checks Kubernetes and OpenShift manifests
// against the JSON Schemas published at https://openshiftjsonschema.dev.
//
// With no flags it talks MCP over stdin/stdout, for an agent to run locally.
// The manifests it checks then never leave the machine; only schemas are
// downloaded.
//
// With -http it serves MCP over HTTP at /mcp, for hosting. Callers need an
// API key from the -keys file, and each key has its own rate limit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "0.1.0"

const instructions = `Checks Kubernetes and OpenShift YAML manifests against the JSON Schemas from openshiftjsonschema.dev.
Call validate with the YAML before you hand a manifest to the user or apply it.
Pass the user's OpenShift version if you know it; otherwise the newest is used.`

// ValidateInput is the validate tool's arguments.
type ValidateInput struct {
	YAML    string `json:"yaml" jsonschema:"the manifest text: one or more YAML documents separated by ---. Pass the content, not a file path"`
	Version string `json:"version,omitempty" jsonschema:"OpenShift version to check against, e.g. 4.22 or 4.22.14. Defaults to the newest. Use list_versions to see what exists"`
	Strict  *bool  `json:"strict,omitempty" jsonschema:"reject fields the schema does not know, which catches misspelt and misplaced fields. Defaults to true"`
}

// Version describes one OpenShift minor in list_versions.
type Version struct {
	OpenShift  string `json:"openshift" jsonschema:"minor version, e.g. 4.22"`
	Kubernetes string `json:"kubernetes,omitempty" jsonschema:"the Kubernetes version it is based on, if known"`
}

// VersionList is what list_versions returns.
type VersionList struct {
	Latest   string    `json:"latest"`
	Versions []Version `json:"versions"`
}

func newServer(cat *catalog, maxInput int) *mcp.Server {
	s := mcp.NewServer(
		&mcp.Implementation{Name: "openshift-json-schema", Title: "OpenShift JSON Schema", Version: version},
		&mcp.ServerOptions{Instructions: instructions},
	)
	v := &validator{cat: cat}
	closed := false
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closed}

	mcp.AddTool(s, &mcp.Tool{
		Name:  "validate",
		Title: "Validate manifests",
		Description: "Check Kubernetes and OpenShift YAML manifests against the JSON Schemas for an OpenShift version. " +
			"Covers the built-in Kubernetes kinds (Deployment, Service, ConfigMap, ...) and the OpenShift ones " +
			"(Route, BuildConfig, ImageStream, DeploymentConfig, ...). " +
			"Returns one result per manifest, with the path and reason for each problem. " +
			"This checks structure and types only: it does not contact a cluster, " +
			"so it cannot tell whether names, images or referenced objects exist.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ValidateInput) (*mcp.CallToolResult, *Report, error) {
		if len(in.YAML) > maxInput {
			return nil, nil, fmt.Errorf("yaml is %d bytes; the limit is %d per call. Split it into smaller calls",
				len(in.YAML), maxInput)
		}
		rep, err := v.validate(ctx, in.YAML, in.Version, in.Strict == nil || *in.Strict)
		return nil, rep, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_versions",
		Title:       "List OpenShift versions",
		Description: "List the OpenShift versions that validate can check against.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *VersionList, error) {
		versions, err := cat.list(ctx)
		if err != nil {
			return nil, nil, err
		}
		out := &VersionList{Latest: versions[len(versions)-1].Minor}
		for _, info := range versions {
			out.Versions = append(out.Versions, Version{OpenShift: info.Minor, Kubernetes: info.Kubernetes})
		}
		return nil, out, nil
	})
	return s
}

func main() {
	schemas := flag.String("schemas", "https://openshiftjsonschema.dev",
		"where the schemas are: the site's URL, or a local copy of its document root")
	maxInput := flag.Int("max-input", 1<<20, "largest YAML input accepted per call, in bytes")
	addr := flag.String("http", "", "serve MCP over HTTP on this address (e.g. 127.0.0.1:8087) instead of stdin/stdout; needs -keys")
	keysFile := flag.String("keys", "", "file of API keys for -http, one \"name sha256\" per line; add lines with -new-key")
	perMinute := flag.Float64("rate", 60, "with -http: requests each API key may make per minute")
	burst := flag.Int("burst", 30, "with -http: requests each API key may make at once before the rate applies")
	newKeyName := flag.String("new-key", "", "make an API key for this name: prints the key, and the line to add to the -keys file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log.SetFlags(0)
	log.SetPrefix("ojs-mcp: ")

	if *newKeyName != "" {
		key, line, err := newKey(*newKeyName)
		if err != nil {
			log.Fatal(err)
		}
		// The line goes to stdout so it can be appended to the keys file;
		// the key itself goes to the terminal only.
		fmt.Fprintf(os.Stderr, "API key for %s (shown once, it is not stored):\n\n  %s\n\n", *newKeyName, key)
		fmt.Println(line)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := newServer(newCatalog(newSource(*schemas)), *maxInput)

	if *addr == "" {
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatal(err)
		}
		return
	}

	if *keysFile == "" {
		log.Fatal("-http needs -keys: the hosted server only answers callers with an API key")
	}
	if *perMinute <= 0 || *burst < 1 {
		log.Fatal("-rate must be above 0 and -burst at least 1")
	}
	keys, err := openKeyStore(*keysFile)
	if err != nil {
		log.Fatalf("reading API keys: %v", err)
	}
	handler := newHTTPHandler(server, keys, newLimiters(*perMinute, *burst), *maxInput)
	log.Printf("serving MCP on http://%s/mcp, schemas from %s, %g requests a minute per key", *addr, *schemas, *perMinute)
	if err := serveHTTP(ctx.Done(), *addr, handler); err != nil {
		log.Fatal(err)
	}
}
