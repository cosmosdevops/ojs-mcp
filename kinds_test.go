package main

import (
	"context"
	"strings"
	"testing"
)

// A kind with no schema is the manifest's mistake when it is a slip in an
// API this version has, and is skipped when it looks like a custom resource.
func TestMissingKind(t *testing.T) {
	tests := []struct {
		name, apiVersion, kind string
		status, path           string
		mentions               string
	}{
		{"typo in an OpenShift kind", "route.openshift.io/v1", "Rout", "invalid", "/kind", "Did you mean Route?"},
		{"typo in a core kind", "v1", "ConfigMapp", "invalid", "/kind", "Did you mean ConfigMap?"},
		{"typo in a built-in kind", "apps/v1", "Deploymnet", "invalid", "/kind", "Did you mean Deployment?"},
		{"typo in an indexed custom resource", "monitoring.coreos.com/v1", "ServiceMonitors", "invalid", "/kind", "Did you mean ServiceMonitor?"},
		{"removed version", "autoscaling/v2beta2", "HorizontalPodAutoscaler", "invalid", "/apiVersion", "autoscaling has it in version v2"},
		{"wrong version of an OpenShift API", "route.openshift.io/v2", "Route", "invalid", "/apiVersion", "route.openshift.io has it in version v1"},
		{"kind from another group in the core group", "v1", "Route", "invalid", "/kind", "exists in another API group"},
		{"OpenShift kind in the Kubernetes group", "apps/v1", "DeploymentConfig", "invalid", "/kind", "exists in another API group"},
		{"made-up kind in a built-in group", "apps/v1", "Widget", "invalid", "/kind", "apiVersion apps/v1 has no kind Widget in OpenShift 4.22."},

		{"operator resource", "cert-manager.io/v1", "Certificate", "skipped", "", "not checked"},
		{"operator resource reusing a kind name", "serving.knative.dev/v1", "Service", "skipped", "", "not checked"},
		// networking.istio.io shares its first label, and so its schema
		// file names, with networking.k8s.io. It must not be mistaken for it.
		{"operator group sharing a label", "networking.istio.io/v1", "Ingres", "skipped", "", "not checked"},
		{"operator group sharing a label and kind", "apps.example.com/v2", "Deployment", "skipped", "", "not checked"},
		{"unknown group without a dot", "example/v1", "Widget", "skipped", "", "not checked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := "apiVersion: " + tt.apiVersion + "\nkind: " + tt.kind + "\n"
			rep, err := testValidator().validate(context.Background(), in, "4.22", true)
			if err != nil {
				t.Fatal(err)
			}
			got := rep.Results[0]
			if got.Status != tt.status {
				t.Fatalf("status = %s (%v), want %s", got.Status, got.Problems, tt.status)
			}
			if p := got.Problems[0]; p.Path != tt.path || !strings.Contains(p.Message, tt.mentions) {
				t.Errorf("problem = %+v, want path %q and a mention of %q", p, tt.path, tt.mentions)
			}
		})
	}
}

// Without all.json there is nothing to compare against, so nothing is
// called a mistake.
func TestMissingKindWithoutIndex(t *testing.T) {
	v := &validator{cat: newCatalog(newSource(t.TempDir()))}
	if idx := v.cat.kinds(context.Background(), "v4.22-next-standalone-strict"); idx != nil {
		t.Fatalf("got an index from an empty directory: %+v", idx)
	}
}

func TestEditDistance(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{{"route", "route", 0}, {"rout", "route", 1}, {"deploymnet", "deployment", 2}, {"", "abc", 3}, {"pod", "job", 2}} {
		if got := editDistance(tt.a, tt.b); got != tt.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
	// Short words get less slack, or every three-letter kind would match.
	if got := closest("Pod", []string{"Job"}); got != "" {
		t.Errorf(`closest("Pod") = %q, want no guess`, got)
	}
}
