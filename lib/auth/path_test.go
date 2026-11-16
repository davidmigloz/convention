package auth_test

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"

	convAuth "github.com/sofmon/convention/lib/auth"
)

func TestPathSegments(t *testing.T) {

	tests := []struct {
		name string
		url  *url.URL
		want []string
		ok   bool
	}{
		{"plain", &url.URL{Path: "/a/b/c"}, []string{"a", "b", "c"}, true},
		{"trailing slash", &url.URL{Path: "/a/b/"}, []string{"a", "b"}, true},
		{"encoded slash stays in its segment", mustParse(t, "/assets/secret%2F@public"), []string{"assets", "secret/@public"}, true},
		{"encoded percent", mustParse(t, "/assets/%252F"), []string{"assets", "%2F"}, true},
		{"encoded space and non ascii", mustParse(t, "/a/x%20y/m%C3%BC"), []string{"a", "x y", "mü"}, true},
		// A raw '"' makes EscapedPath() re-encode Path, which would lose the %2F.
		{"raw path that EscapedPath would drop", &url.URL{Path: `/assets/x/@p"`, RawPath: `/assets/x%2F@p"`}, []string{"assets", `x/@p"`}, true},
		// A RawPath that does not decode to Path is ignored.
		{"inconsistent raw path ignored", &url.URL{Path: "/a/b", RawPath: "/a%2Fc"}, []string{"a", "b"}, true},
		{"nil url", nil, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := convAuth.PathSegments(tt.url)
			if ok != tt.ok || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PathSegments = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestCheck_encodedSlashDoesNotMatchAnotherAction: the policy must evaluate
// the route the API router runs. An encoded '/' stays inside its segment, so
// it cannot make a request look like a longer, public action.
func TestCheck_encodedSlashDoesNotMatchAnotherAction(t *testing.T) {

	check, err := convAuth.NewCheck(convAuth.Policy{
		Roles: convAuth.RolePermissions{
			"reader": convAuth.Permissions{"read_assets"},
		},
		Permissions: convAuth.PermissionActions{
			"read_assets": convAuth.Actions{"GET /x/v1/assets/{any}"},
		},
		Public: convAuth.Actions{"GET /x/v1/assets/{any}/@public"},
	})
	if err != nil {
		t.Fatalf("NewCheck: %v", err)
	}

	public := &http.Request{Method: "GET", URL: mustParse(t, "/x/v1/assets/secret/@public")}
	if _, err := check(public); err != nil {
		t.Errorf("public action refused: %v", err)
	}

	encoded := &http.Request{Method: "GET", URL: mustParse(t, "/x/v1/assets/secret%2F@public"), Header: http.Header{}}
	if _, err := check(encoded); err == nil {
		t.Errorf("unauthenticated %s passed the check as the public action", encoded.URL.RawPath)
	}
}

// TestCheck_policySegmentMatchesOnePlainSegment: an open-route handler reads
// the decoded path, so a policy segment must not match a segment that decodes
// to more than one ('/' or '\'), or to '.' or '..'. Otherwise a narrow public
// grant such as {any}/hero.webp would cover any depth below it.
func TestCheck_policySegmentMatchesOnePlainSegment(t *testing.T) {

	check, err := convAuth.NewCheck(convAuth.Policy{
		Roles: convAuth.RolePermissions{
			"reader": convAuth.Permissions{"read_files"},
		},
		Permissions: convAuth.PermissionActions{
			"read_files": convAuth.Actions{"GET /x/v1/files/{any...}"},
		},
		Public: convAuth.Actions{"GET /x/v1/files/{any}/hero.webp"},
	})
	if err != nil {
		t.Fatalf("NewCheck: %v", err)
	}

	allowed := []string{
		"/x/v1/files/pub/hero.webp",
		"/x/v1/files/a..b/hero.webp",
	}
	refused := []string{
		"/x/v1/files/private%2Fdoc/hero.webp",
		"/x/v1/files/..%2Fx/hero.webp",
		"/x/v1/files/pub%2F..%2F..%2Fother/hero.webp",
		"/x/v1/files/a%5Cb/hero.webp",
		"/x/v1/files/../hero.webp",
		"/x/v1/files/./hero.webp",
		"/x/v1/files/%2E%2E/hero.webp",
	}

	for _, raw := range allowed {
		req := &http.Request{Method: "GET", URL: mustParse(t, raw), Header: http.Header{}}
		if _, err := check(req); err != nil {
			t.Errorf("unauthenticated %s refused: %v", raw, err)
		}
	}
	for _, raw := range refused {
		req := &http.Request{Method: "GET", URL: mustParse(t, raw), Header: http.Header{}}
		if _, err := check(req); err == nil {
			t.Errorf("unauthenticated %s passed the public action", raw)
		}
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
