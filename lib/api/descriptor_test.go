package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
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
		{"already percent encoded slash", "%2F", "%252F"},
		{"percent", "100%", "100%25"},
		{"question mark", "a?b=c", "a%3Fb=c"},
		{"hash", "a#b", "a%23b"},
		{"space", "a b", "a%20b"},
		{"non ascii", "müller-ü", "m%C3%BCller-%C3%BC"},
		{"dots inside a value", "a..b", "a..b"},
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

func Test_descriptor_newRequest_refusesValuesThatAreNotOneSegment(t *testing.T) {

	desc := newDescriptor("localhost", 443, "GET /assets/{id}", nil, nil)

	for _, v := range []string{
		"a/b", "/", "x/@projection", "../../other/x",
		".", "..",
		`a\b`,
		"a\nb", "a\rb", "a\x00b", "a\x7fb", "a\tb",
	} {
		t.Run(strconv.Quote(v), func(t *testing.T) {
			vls := values{}
			vls.Add("", v)

			req, err := desc.newRequest(vls, nil)
			if !errors.Is(err, ErrInvalidPathValue) {
				t.Errorf("newRequest(%q) error = %v, want ErrInvalidPathValue", v, err)
			}
			if req != nil {
				t.Errorf("newRequest(%q) built a request for %s", v, req.URL)
			}
		})
	}
}

func Test_descriptor_match_refusesParamsThatAreNotOneSegment(t *testing.T) {

	desc := newDescriptor("localhost", 443, "GET /assets/{id}", nil, nil)
	sibling := newDescriptor("localhost", 443, "GET /assets/{id}/@projection", nil, nil)
	open := newDescriptor("localhost", 443, "GET /assets/{id}/{any...}", nil, nil)

	refused := []string{
		"/assets/x%2F@projection",
		"/assets/..%2Fother",
		"/assets/%2F",
		"/assets/a%5Cb",
		"/assets/a%00b",
		"/assets/a%0Ab",
		"/assets/.",
		"/assets/..",
		"/assets/%2E%2E",
	}

	for _, raw := range refused {
		t.Run(raw, func(t *testing.T) {
			u, err := url.ParseRequestURI(raw)
			if err != nil {
				t.Fatalf("parse %q: %v", raw, err)
			}
			req := &http.Request{Method: "GET", URL: u}

			for _, d := range []descriptor{desc, sibling, open} {
				if vls, m := d.match(req); m {
					t.Errorf("%s matched %s with %v", raw, d.path(), vls)
				}
			}
		})
	}

	// A real '/' still separates segments, as before.
	req := &http.Request{Method: "GET", URL: &url.URL{Path: "/assets/x/@projection"}}
	if _, m := desc.match(req); m {
		t.Errorf("unescaped slash should not match %s", desc.path())
	}
	if vls, m := sibling.match(req); !m || vls.GetByIndex(0) != "x" {
		t.Errorf("unescaped slash should match %s with id x, got match=%v vals=%v", sibling.path(), m, vls)
	}

	// A hand-written target whose raw form is not a valid encoding (a raw '"')
	// is still split on its raw path, so the encoded '/' is refused, not split.
	req = &http.Request{Method: "GET", URL: &url.URL{Path: `/assets/x/@projection"`, RawPath: `/assets/x%2F@projection"`}}
	for _, d := range []descriptor{desc, sibling} {
		if vls, m := d.match(req); m {
			t.Errorf("raw %q matched %s with %v", req.URL.RawPath, d.path(), vls)
		}
	}
}
