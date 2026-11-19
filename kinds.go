package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A kindIndex lists the types a schema directory has. It is read from the
// directory's all.json, which names every definition, e.g.
// "com.github.openshift.api.route.v1.Route". The last three parts are what
// schema file names are built from: the first label of the API group
// ("route", or "core" for the group-less v1), the version and the kind. The
// first parts say whose API it is.
type kindIndex struct {
	byGroupVersion map[string][]indexedKind // "route/v1" -> its kinds, sorted
}

type indexedKind struct {
	kind   string
	domain string // what the API group ends with: k8s.io, openshift.io, ...
}

var apiVersionPart = regexp.MustCompile(`^v[0-9]+((alpha|beta)[0-9]+)?$`)

func parseKindIndex(allJSON []byte) (*kindIndex, error) {
	var doc struct {
		OneOf []struct {
			Ref string `json:"$ref"`
		} `json:"oneOf"`
	}
	if err := json.Unmarshal(allJSON, &doc); err != nil {
		return nil, err
	}
	idx := &kindIndex{byGroupVersion: map[string][]indexedKind{}}
	for _, entry := range doc.OneOf {
		_, name, _ := strings.Cut(entry.Ref, "#/definitions/")
		parts := strings.Split(name, ".")
		if len(parts) < 5 || !apiVersionPart.MatchString(parts[len(parts)-2]) {
			continue // a shared helper type, such as IntOrString
		}
		gv := parts[len(parts)-3] + "/" + parts[len(parts)-2]
		idx.byGroupVersion[gv] = append(idx.byGroupVersion[gv],
			indexedKind{kind: parts[len(parts)-1], domain: definitionDomain(parts)})
	}
	for _, kinds := range idx.byGroupVersion {
		sort.Slice(kinds, func(i, j int) bool { return kinds[i].kind < kinds[j].kind })
	}
	return idx, nil
}

// definitionDomain works out what the API group of a definition ends with.
// Custom resources are named by their group backwards (com.coreos.monitoring
// is monitoring.coreos.com); built-in types are named by Go package.
func definitionDomain(parts []string) string {
	switch strings.Join(parts[:3], ".") {
	case "com.github.openshift":
		return "openshift.io"
	case "com.github.operator-framework":
		return "coreos.com"
	}
	return parts[1] + "." + parts[0]
}

// An apiGroup is an apiVersion split into the parts schema names use.
type apiGroup struct {
	name    string // "route.openshift.io"; empty for the core group
	label   string // its first label, "route"; "core" for the core group
	version string
}

func parseAPIGroup(apiVersion string) apiGroup {
	group, version, hasGroup := strings.Cut(strings.ToLower(apiVersion), "/")
	if !hasGroup {
		return apiGroup{label: "core", version: group}
	}
	label, _, _ := strings.Cut(group, ".")
	return apiGroup{name: group, label: label, version: version}
}

// builtIn reports a group with no dot in it (v1, apps/v1, batch/v1). Only
// Kubernetes itself defines those.
func (g apiGroup) builtIn() bool { return !strings.Contains(g.name, ".") }

// owns reports whether an indexed kind can belong to this group. Schema
// names only keep a group's first label, so networking.k8s.io and
// networking.istio.io look alike; the domain tells them apart.
func (g apiGroup) owns(k indexedKind) bool {
	if g.builtIn() {
		return k.domain == "k8s.io"
	}
	return strings.HasSuffix(g.name, "."+k.domain)
}

// kindsIn lists the group's kinds in one version.
func (idx *kindIndex) kindsIn(g apiGroup, version string) []string {
	var out []string
	for _, k := range idx.byGroupVersion[g.label+"/"+version] {
		if g.owns(k) {
			out = append(out, k.kind)
		}
	}
	return out
}

// versions lists the versions of the group that are indexed.
func (idx *kindIndex) versions(g apiGroup) []string {
	var out []string
	for gv := range idx.byGroupVersion {
		label, version, _ := strings.Cut(gv, "/")
		if label == g.label && len(idx.kindsIn(g, version)) > 0 {
			out = append(out, version)
		}
	}
	sort.Strings(out)
	return out
}

// versionsWith lists the versions of the group that have the kind.
func (idx *kindIndex) versionsWith(g apiGroup, kind string) []string {
	var out []string
	for _, version := range idx.versions(g) {
		if containsFold(idx.kindsIn(g, version), kind) {
			out = append(out, version)
		}
	}
	return out
}

// hasElsewhere reports whether any group at all has the kind.
func (idx *kindIndex) hasElsewhere(kind string) bool {
	for _, kinds := range idx.byGroupVersion {
		for _, k := range kinds {
			if strings.EqualFold(k.kind, kind) {
				return true
			}
		}
	}
	return false
}

func containsFold(list []string, s string) bool {
	for _, item := range list {
		if strings.EqualFold(item, s) {
			return true
		}
	}
	return false
}

// closest returns the candidate that a typo most likely meant, or "".
func closest(word string, candidates []string) string {
	limit := 2
	if len(word) < 5 {
		limit = 1
	}
	best, bestDist := "", limit+1
	for _, c := range candidates {
		if d := editDistance(strings.ToLower(word), strings.ToLower(c)); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between two strings.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// explainMissing decides what a missing schema means. A kind that looks
// like a slip in an API this version does have is the manifest's mistake
// (ok is true, with the problem to report). Anything else is most likely a
// custom resource from an operator, which there are no schemas for.
func (idx *kindIndex) explainMissing(kind, apiVersion, openshift string) (p Problem, ok bool) {
	g := parseAPIGroup(apiVersion)
	where := fmt.Sprintf("apiVersion %s has no kind %s in OpenShift %s.", apiVersion, kind, openshift)

	if guess := closest(kind, idx.kindsIn(g, g.version)); guess != "" {
		return Problem{Path: "/kind", Message: where + " Did you mean " + guess + "?"}, true
	}
	if versions := idx.versionsWith(g, kind); len(versions) > 0 {
		name := g.name
		if name == "" {
			name = "The core group"
		}
		return Problem{Path: "/apiVersion", Message: fmt.Sprintf(
			"%s %s has it in version %s.", where, name, strings.Join(versions, ", "))}, true
	}
	// A custom resource can share a built-in group's first label, but it
	// can't be in a built-in group itself, so there this is always a mistake.
	if g.builtIn() && len(idx.versions(g)) > 0 {
		if idx.hasElsewhere(kind) {
			where += " A kind with that name exists in another API group; check the apiVersion."
		}
		return Problem{Path: "/kind", Message: where}, true
	}
	return Problem{}, false
}

// kinds returns the index of a schema directory, or nil if it has none.
func (c *catalog) kinds(ctx context.Context, dir string) *kindIndex {
	c.mu.Lock()
	idx, ok := c.indexes[dir]
	c.mu.Unlock()
	if ok {
		return idx
	}
	if b, err := c.src.read(ctx, dir+"/all.json"); err == nil {
		idx, _ = parseKindIndex(b)
	}
	c.mu.Lock()
	c.indexes[dir] = idx // nil is remembered too: don't ask again
	c.mu.Unlock()
	return idx
}
