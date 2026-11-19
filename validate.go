package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const (
	maxDocuments    = 200
	maxErrorsPerDoc = 50
)

// Problem is one thing wrong with a manifest.
type Problem struct {
	Path    string `json:"path,omitempty" jsonschema:"where in the manifest, as a JSON pointer such as /spec/to/kind; empty for the whole document"`
	Message string `json:"message"`
}

// Result is the outcome for one manifest.
type Result struct {
	Document   int       `json:"document" jsonschema:"position of the YAML document in the input, starting at 1"`
	Kind       string    `json:"kind,omitempty"`
	APIVersion string    `json:"apiVersion,omitempty"`
	Name       string    `json:"name,omitempty"`
	Namespace  string    `json:"namespace,omitempty"`
	Status     string    `json:"status" jsonschema:"valid, invalid (including a kind or apiVersion this OpenShift version does not have), skipped (no schema, so it was not checked; usual for custom resources from operators) or error (could not be read)"`
	Problems   []Problem `json:"problems,omitempty"`
}

// Summary counts results by status.
type Summary struct {
	Valid   int `json:"valid"`
	Invalid int `json:"invalid"`
	Skipped int `json:"skipped"`
	Errors  int `json:"errors"`
}

// Report is what the validate tool returns.
type Report struct {
	OpenShift string   `json:"openshift" jsonschema:"the OpenShift minor version the manifests were checked against"`
	Schemas   string   `json:"schemas" jsonschema:"the schema set used, e.g. v4.22-next-standalone-strict"`
	Summary   Summary  `json:"summary"`
	Results   []Result `json:"results"`
}

type validator struct {
	cat *catalog
}

// validate checks every manifest in a YAML stream against one version.
func (v *validator) validate(ctx context.Context, input, wantVersion string, strict bool) (*Report, error) {
	info, err := v.cat.resolve(ctx, wantVersion)
	if err != nil {
		return nil, err
	}
	_, dir, ok := info.flavour(strict)
	if !ok {
		return nil, fmt.Errorf("OpenShift %s has no standalone schemas", info.Minor)
	}
	rep := &Report{OpenShift: info.Minor, Schemas: dir, Results: []Result{}}

	dec := yaml.NewDecoder(strings.NewReader(input))
	for n := 1; ; n++ {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// The decoder can't carry on after a syntax error.
			rep.add(Result{Document: n, Status: "error",
				Problems: []Problem{{Message: "not valid YAML: " + err.Error()}}})
			break
		}
		if doc == nil {
			continue // empty document, e.g. a trailing ---
		}
		if n > maxDocuments {
			return nil, fmt.Errorf("too many documents: the limit is %d per call", maxDocuments)
		}
		for _, obj := range expandList(doc) {
			rep.add(v.check(ctx, n, obj, dir, info.Minor))
		}
	}
	if len(rep.Results) == 0 {
		return nil, errors.New("the input has no YAML documents")
	}
	return rep, nil
}

func (r *Report) add(res Result) {
	switch res.Status {
	case "valid":
		r.Summary.Valid++
	case "invalid":
		r.Summary.Invalid++
	case "skipped":
		r.Summary.Skipped++
	default:
		r.Summary.Errors++
	}
	r.Results = append(r.Results, res)
}

// expandList returns the items of a "kind: List" object, or the object
// itself.
func expandList(doc any) []any {
	m, ok := doc.(map[string]any)
	if !ok {
		return []any{doc}
	}
	items, isList := m["items"].([]any)
	if kind, _ := m["kind"].(string); kind != "List" || !isList {
		return []any{doc}
	}
	return items
}

func (v *validator) check(ctx context.Context, n int, doc any, dir, openshift string) Result {
	res := Result{Document: n}
	fail := func(status, msg string) Result {
		res.Status = status
		res.Problems = []Problem{{Message: msg}}
		return res
	}

	obj, ok := doc.(map[string]any)
	if !ok {
		return fail("error", "not a Kubernetes object: expected a mapping with apiVersion and kind")
	}
	res.Kind, _ = obj["kind"].(string)
	res.APIVersion, _ = obj["apiVersion"].(string)
	if meta, ok := obj["metadata"].(map[string]any); ok {
		res.Name, _ = meta["name"].(string)
		res.Namespace, _ = meta["namespace"].(string)
	}
	if res.Kind == "" {
		return fail("error", "missing kind")
	}
	if res.APIVersion == "" {
		return fail("error", "missing apiVersion")
	}
	file, ok := schemaFile(res.Kind, res.APIVersion)
	if !ok {
		return fail("error", "kind or apiVersion has characters that no Kubernetes API uses")
	}

	schema, err := v.cat.schema(ctx, dir, file)
	if errors.Is(err, errNotFound) {
		if idx := v.cat.kinds(ctx, dir); idx != nil {
			if p, ok := idx.explainMissing(res.Kind, res.APIVersion, openshift); ok {
				res.Status = "invalid"
				res.Problems = []Problem{p}
				return res
			}
		}
		return fail("skipped", fmt.Sprintf(
			"not checked: no schema for kind %s in apiVersion %s. This is normal for custom resources "+
				"from operators that are not part of a base OpenShift install; otherwise check both for typos",
			res.Kind, res.APIVersion))
	}
	if err != nil {
		return fail("error", "could not load the schema: "+err.Error())
	}

	// Go through JSON so the validator sees the types it expects.
	b, err := json.Marshal(obj)
	if err != nil {
		return fail("error", "not a Kubernetes object: "+err.Error())
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return fail("error", err.Error())
	}

	err = schema.Validate(inst)
	var verr *jsonschema.ValidationError
	switch {
	case err == nil:
		res.Status = "valid"
	case errors.As(err, &verr):
		res.Status = "invalid"
		res.Problems = problems(verr)
	default:
		return fail("error", err.Error())
	}
	return res
}

var apiName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// schemaFile names the schema for a kind the way kubeconform does:
// Route in route.openshift.io/v1 is route-route-v1.json, ConfigMap in v1 is
// configmap-v1.json. It refuses anything that could not be such a name,
// which also keeps the result inside the schema directory.
func schemaFile(kind, apiVersion string) (string, bool) {
	parts := strings.Split(strings.ToLower(apiVersion), "/")
	if len(parts) > 2 {
		return "", false
	}
	names := []string{strings.ToLower(kind), strings.Split(parts[0], ".")[0]}
	names = append(names, parts[1:]...)
	for _, n := range names {
		if !apiName.MatchString(n) {
			return "", false
		}
	}
	return strings.Join(names, "-") + ".json", true
}

var printer = message.NewPrinter(language.English)

// problems flattens a validation error into its most specific causes.
func problems(e *jsonschema.ValidationError) []Problem {
	var out []Problem
	seen := map[Problem]bool{}
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		p := Problem{Message: e.ErrorKind.LocalizedString(printer)}
		if len(e.InstanceLocation) > 0 {
			p.Path = "/" + strings.Join(e.InstanceLocation, "/")
		}
		if !seen[p] && len(out) < maxErrorsPerDoc {
			seen[p] = true
			out = append(out, p)
		}
	}
	walk(e)
	return out
}
