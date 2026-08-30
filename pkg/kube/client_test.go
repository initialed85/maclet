package kube

import (
	"net/url"
	"testing"
)

func TestNormalizeServer(t *testing.T) {
	got, err := NormalizeServer("https://example.test:6443/")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "https://example.test:6443" {
		t.Fatalf("normalized URL = %q", got)
	}
	for _, server := range []string{"", "http://example.test:6443", "https:///missing-host"} {
		if _, err := NormalizeServer(server); err == nil {
			t.Errorf("NormalizeServer(%q) accepted invalid URL", server)
		}
	}
}

func TestClientEndpoint(t *testing.T) {
	client, err := NewClientWithMaterial("https://example.test:6443/base", []byte("unused"), nil, nil, true, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	got := client.endpoint("/api/v1/nodes?watch=true")
	if got != "https://example.test:6443/api/v1/nodes?watch=true" {
		t.Fatalf("endpoint = %q", got)
	}
	if _, err := url.Parse(got); err != nil {
		t.Fatal(err)
	}
}
