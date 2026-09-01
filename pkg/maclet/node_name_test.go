package maclet

import (
	"strings"
	"testing"
)

func TestNodeNameFromHostname(t *testing.T) {
	tests := map[string]string{
		"MacBook-Pro.local":   "macbook-pro.local",
		"Mac.ts.net lan":      "mac.ts.net-lan",
		" host_name ":         "host-name",
		".leading..trailing.": "leading.trailing",
		"---":                 "",
	}
	for hostname, want := range tests {
		t.Run(hostname, func(t *testing.T) {
			if got := nodeNameFromHostname(hostname); got != want {
				t.Fatalf("nodeNameFromHostname(%q) = %q, want %q", hostname, got, want)
			}
		})
	}
}

func TestNodeNameFromHostnameLimitsLabelsAndName(t *testing.T) {
	longLabel := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	longHostname := longLabel + "." + longLabel + "." + longLabel + "." + longLabel
	got := nodeNameFromHostname(longHostname)
	if len(got) > 253 {
		t.Fatalf("node name length = %d, want <= 253", len(got))
	}
	for _, label := range splitNodeNameLabels(got) {
		if len(label) > 63 {
			t.Fatalf("node label %q length = %d, want <= 63", label, len(label))
		}
	}
}

func splitNodeNameLabels(name string) []string {
	return strings.Split(name, ".")
}
