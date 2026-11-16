package api_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	convAPI "github.com/sofmon/convention/lib/api"
	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
)

type escapeAPI struct {
	GetAsset  convAPI.OutP1[string, string] `api:"GET /escape/v1/assets/{id}"`
	GetPublic convAPI.OutP1[string, string] `api:"GET /escape/v1/assets/{id}/@public"`
}

// Test_path_values_cannot_change_the_route runs a real client against a real
// handler with a real authorization policy: the @public route is public and
// the plain asset route needs a role. A path value can be escaped and sent,
// or refused, but it never reaches a route other than the one called, and the
// policy always evaluates the route that runs.
//
// The client sends through http.DefaultClient, so this test swaps its
// transport and must not run in parallel with other tests in this package.
func Test_path_values_cannot_change_the_route(t *testing.T) {

	const roleReader convAuth.Role = "escape_reader"

	var protectedCalls atomic.Int32

	impl := escapeAPI{
		GetAsset: convAPI.NewOutP1(func(ctx convCtx.Context, id string) (string, error) {
			protectedCalls.Add(1)
			return "protected:" + id, nil
		}),
		GetPublic: convAPI.NewOutP1(func(ctx convCtx.Context, id string) (string, error) {
			return "public:" + id, nil
		}),
	}

	check, err := convAuth.NewCheck(convAuth.Policy{
		Roles: convAuth.RolePermissions{
			roleReader: convAuth.Permissions{"read_assets"},
		},
		Permissions: convAuth.PermissionActions{
			"read_assets": convAuth.Actions{"GET /escape/v1/assets/{any}"},
		},
		Public: convAuth.Actions{"GET /escape/v1/assets/{any}/@public"},
	})
	if err != nil {
		t.Fatalf("NewCheck: %v", err)
	}

	srvCtx := convCtx.New(convAuth.Claims{User: "escape-server"})
	srv := httptest.NewTLSServer(convAPI.NewHandler(srvCtx, "127.0.0.1", 0, check, &impl))
	defer srv.Close()

	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	defer func() { http.DefaultClient.Transport = prev }()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	client := convAPI.NewClient[escapeAPI](u.Hostname(), port)

	reader := convCtx.New(convAuth.Claims{User: "escape-reader", Roles: convAuth.Roles{roleReader}})

	t.Run("unauthenticated encoded slash does not reach the protected handler", func(t *testing.T) {
		before := protectedCalls.Load()

		res, err := srv.Client().Get(srv.URL + "/escape/v1/assets/secret%2F@public")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()

		if res.StatusCode == http.StatusOK {
			t.Errorf("status = 200 with body %q, want a refusal", body)
		}
		if protectedCalls.Load() != before {
			t.Errorf("protected handler ran for an unauthenticated request")
		}
	})

	t.Run("public route still works", func(t *testing.T) {
		got, err := client.GetPublic.Call(convCtx.New(convAuth.Claims{}), "secret")
		if err != nil || got != "public:secret" {
			t.Errorf("GetPublic.Call = %q, %v; want %q", got, err, "public:secret")
		}
	})

	for _, id := range []string{"abc-123", "%2F", "a?b=c", "a#b", "a b", "müller", "first+tag@example.com"} {
		t.Run("sent escaped "+strconv.Quote(id), func(t *testing.T) {
			got, err := client.GetAsset.Call(reader, id)
			if err != nil {
				t.Fatalf("GetAsset.Call(%q) error: %v", id, err)
			}
			if want := "protected:" + id; got != want {
				t.Errorf("GetAsset.Call(%q) = %q, want %q", id, got, want)
			}
		})
	}

	for _, id := range []string{"a/b", "secret/@public", "/", "..", ".", "../../other", `a\b`, "a\x00b"} {
		t.Run("refused "+strconv.Quote(id), func(t *testing.T) {
			before := protectedCalls.Load()

			_, err := client.GetAsset.Call(reader, id)
			if !errors.Is(err, convAPI.ErrInvalidPathValue) {
				t.Errorf("GetAsset.Call(%q) error = %v, want ErrInvalidPathValue", id, err)
			}
			if protectedCalls.Load() != before {
				t.Errorf("GetAsset.Call(%q) reached the handler", id)
			}
		})
	}
}
