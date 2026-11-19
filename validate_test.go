package main

import (
	"context"
	"strings"
	"testing"
)

func testValidator() *validator {
	return &validator{cat: newCatalog(newSource("testdata/site"))}
}

const route = "apiVersion: route.openshift.io/v1\nkind: Route\nmetadata:\n  name: r\nspec:\n"

func TestValidate(t *testing.T) {
	tests := []struct {
		name, yaml string
		strict     bool
		want       string // status of the first result
		problem    string // text its first problem must contain
	}{
		{"valid", route + "  to: {}\n  port: 8080\n", true, "valid", ""},
		{"unknown field, strict", route + "  to: {}\n  hostname: x\n", true, "invalid", "hostname"},
		{"unknown field, loose", route + "  to: {}\n  hostname: x\n", false, "valid", ""},
		{"wrong type", route + "  to: {}\n  port: http\n", true, "invalid", "want integer"},
		{"missing field", route + "  port: 1\n", true, "invalid", "'to'"},
		{"in a List", "kind: List\napiVersion: v1\nitems:\n- " + strings.ReplaceAll(route+"  to: {}\n", "\n", "\n  "), true, "valid", ""},
		{"unknown kind", "apiVersion: example.com/v1\nkind: Widget\n", true, "skipped", "no schema"},
		{"path traversal", "apiVersion: ../../v1\nkind: ../x\n", true, "error", "characters"},
		{"no kind", "apiVersion: v1\n", true, "error", "missing kind"},
		{"not an object", "- a\n", true, "error", "not a Kubernetes object"},
		{"bad yaml", "a: [b\n", true, "error", "not valid YAML"},
		{"duplicate key", route + "  to: {}\n  to: {}\n", true, "error", "already defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep, err := testValidator().validate(context.Background(), tt.yaml, "4.22.14", tt.strict)
			if err != nil {
				t.Fatal(err)
			}
			got := rep.Results[0]
			if got.Status != tt.want {
				t.Fatalf("status = %s (%v), want %s", got.Status, got.Problems, tt.want)
			}
			if tt.problem != "" && !strings.Contains(got.Problems[0].Message, tt.problem) {
				t.Errorf("problem = %q, want it to mention %q", got.Problems[0].Message, tt.problem)
			}
		})
	}
}

func TestMultipleDocuments(t *testing.T) {
	in := route + "  to: {}\n---\n---\n" + route + "  bad: 1\n"
	rep, err := testValidator().validate(context.Background(), in, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OpenShift != "4.22" || rep.Schemas != "v4.22-next-standalone-strict" {
		t.Errorf("checked against %s (%s), want the newest", rep.OpenShift, rep.Schemas)
	}
	if rep.Summary != (Summary{Valid: 1, Invalid: 1}) {
		t.Errorf("summary = %+v", rep.Summary)
	}
	if rep.Results[1].Document != 3 || rep.Results[1].Problems[0].Path != "/spec" {
		t.Errorf("second result = %+v", rep.Results[1])
	}
}

func TestVersionErrors(t *testing.T) {
	v := testValidator()
	if _, err := v.validate(context.Background(), route, "3.11", true); err == nil || !strings.Contains(err.Error(), "4.9, 4.22") {
		t.Errorf("unknown version: err = %v", err)
	}
	if _, err := v.validate(context.Background(), route, "4.9", true); err == nil {
		t.Error("4.9 has no strict schemas here, want an error")
	}
	if _, err := v.validate(context.Background(), "", "", true); err == nil {
		t.Error("empty input: want an error")
	}
}

func TestSchemaFile(t *testing.T) {
	for in, want := range map[string]string{
		"Route route.openshift.io/v1": "route-route-v1.json",
		"ConfigMap v1":                "configmap-v1.json",
		"Deployment apps/v1":          "deployment-apps-v1.json",
	} {
		kind, apiVersion, _ := strings.Cut(in, " ")
		if got, _ := schemaFile(kind, apiVersion); got != want {
			t.Errorf("schemaFile(%s) = %s, want %s", in, got, want)
		}
	}
}
