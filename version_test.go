package main

import "testing"

func TestResolveVersion(t *testing.T) {
	for _, tt := range []struct {
		name, fromBuild, fromModule, want string
	}{
		{"go install of a tag", "", "v1.2.3", "1.2.3"},
		{"container build of a tag", "1.2.3", "", "1.2.3"},
		{"container build given a v", "v1.2.3", "", "1.2.3"},
		{"the build's version wins", "1.2.3", "v9.9.9", "1.2.3"},
		{"checkout between tags", "", "v0.1.1-0.20261120010203-11e5561abcde", "0.1.1-0.20261120010203-11e5561abcde"},
		{"no version control", "", "(devel)", "dev"},
		{"nothing known", "", "", "dev"},
		{"blank build argument", " ", "v1.2.3", "1.2.3"},
	} {
		if got := resolveVersion(tt.fromBuild, tt.fromModule); got != tt.want {
			t.Errorf("%s: resolveVersion(%q, %q) = %q, want %q", tt.name, tt.fromBuild, tt.fromModule, got, tt.want)
		}
	}
}
