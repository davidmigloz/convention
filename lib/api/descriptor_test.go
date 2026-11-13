package api

import (
	"net/http"
	"net/url"
	"testing"
)

func Test_descriptor(t *testing.T) {

	match := map[string]*http.Request{
		"GET /api/v1/p1":             {Method: "GET", URL: &url.URL{Path: "/api/v1/p1"}},
		"GET /api/v1/{any...}":       {Method: "GET", URL: &url.URL{Path: "/api/v1/p2"}},
		"GET /{any...}":              {Method: "GET", URL: &url.URL{Path: "/anything/else"}},
		"GET /api/{value1}/{any...}": {Method: "GET", URL: &url.URL{Path: "/api/v1/p2/p3/p4/p5"}},
		"GET /api/{value2}/{any...}": {Method: "GET", URL: &url.URL{Path: "/api/v1/"}},
	}

	notMatch := map[string]*http.Request{
		"GET /api/v1/any":            {Method: "POST", URL: &url.URL{Path: "/api/v1/any"}},
		"GET /api/v2/{any...}":       {Method: "GET", URL: &url.URL{Path: "/api/v1/p2"}},
		"GET /api/{any...}":          {Method: "GET", URL: &url.URL{Path: "/anything/else"}},
		"GET /api/{value2}/{any...}": {Method: "GET", URL: &url.URL{Path: "/api/"}},
	}

	for path, req := range match {
		t.Run(path, func(t *testing.T) {
			desc := newDescriptor("localhost", 443, path, nil, nil)

			if _, m := desc.match(req); !m {
				t.Errorf("expected match")
			}
		})
	}

	for path, req := range notMatch {
		t.Run(path, func(t *testing.T) {
			desc := newDescriptor("localhost", 443, path, nil, nil)

			if _, m := desc.match(req); m {
				t.Errorf("expected not match")
			}
		})
	}

}

func Test_descriptor_newRequest_escapesPathValues(t *testing.T) {

	tests := []struct {
		name    string
		value   string
		escaped string // expected escaped path segment on the wire
	}{
		{"plain id", "abc-123_X.y~", "abc-123_X.y~"},
		{"id with colons", "entity:member-invitation:1:2", "entity:member-invitation:1:2"},
		{"email", "first.last+tag@example.com", "first.last+tag@example.com"},
		{"slash", "a/b", "a%2Fb"},
		{"slash into sibling route", "x/@projection", "x%2F@projection"},
		{"already percent encoded slash", "%2F", "%252F"},
		{"percent", "100%", "100%25"},
		{"question mark", "a?b=c", "a%3Fb=c"},
		{"hash", "a#b", "a%23b"},
		{"space", "a b", "a%20b"},
		{"non ascii", "müller-ü", "m%C3%BCller-%C3%BC"},
		{"dot dot", "..", ".."},
	}

	client := newDescriptor("localhost", 443, "GET /assets/{id}", nil, nil)
	sibling := newDescriptor("localhost", 443, "GET /assets/{id}/@projection", nil, nil)
	listAll := newDescriptor("localhost", 443, "GET /assets/", nil, nil)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vls := values{}
			vls.Add("", tt.value)

			req, err := client.newRequest(vls, nil)
			if err != nil {
				t.Fatalf("newRequest: %v", err)
			}

			if got, want := req.URL.EscapedPath(), "/assets/"+tt.escaped; got != want {
				t.Errorf("escaped path = %q, want %q", got, want)
			}
			if req.URL.RawQuery != "" || req.URL.Fragment != "" {
				t.Errorf("value leaked into query %q or fragment %q", req.URL.RawQuery, req.URL.Fragment)
			}

			// The server must land on the same route with the same value.
			got, m := client.match(req)
			if !m {
				t.Fatalf("request did not match its own route")
			}
			if got.GetByIndex(0) != tt.value {
				t.Errorf("server value = %q, want %q", got.GetByIndex(0), tt.value)
			}
			if _, m := sibling.match(req); m {
				t.Errorf("request matched sibling route %s", sibling.path())
			}
			if _, m := listAll.match(req); m {
				t.Errorf("request matched list route %s", listAll.path())
			}
		})
	}
}

func Test_descriptor_match_keepsEscapedSlashInSegment(t *testing.T) {

	desc := newDescriptor("localhost", 443, "GET /assets/{id}", nil, nil)
	sibling := newDescriptor("localhost", 443, "GET /assets/{id}/@projection", nil, nil)

	// What net/http builds for an incoming "GET /assets/x%2F@projection".
	req := &http.Request{Method: "GET", URL: &url.URL{Path: "/assets/x/@projection", RawPath: "/assets/x%2F@projection"}}

	vls, m := desc.match(req)
	if !m {
		t.Fatalf("expected match on %s", desc.path())
	}
	if got := vls.GetByIndex(0); got != "x/@projection" {
		t.Errorf("value = %q, want %q", got, "x/@projection")
	}
	if _, m := sibling.match(req); m {
		t.Errorf("escaped slash split into a new segment and matched %s", sibling.path())
	}

	// A real '/' still separates segments.
	req = &http.Request{Method: "GET", URL: &url.URL{Path: "/assets/x/@projection"}}
	if _, m := desc.match(req); m {
		t.Errorf("unescaped slash should not match %s", desc.path())
	}
	if vls, m := sibling.match(req); !m || vls.GetByIndex(0) != "x" {
		t.Errorf("unescaped slash should match %s with id x, got match=%v vals=%v", sibling.path(), m, vls)
	}
}
