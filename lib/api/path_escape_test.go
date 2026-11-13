package api_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	convAPI "github.com/sofmon/convention/lib/api"
	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
)

type escapeAPI struct {
	GetAsset      convAPI.OutP1[string, string] `api:"GET /escape/v1/assets/{id}"`
	GetProjection convAPI.OutP1[string, string] `api:"GET /escape/v1/assets/{id}/@projection"`
	ListAssets    convAPI.Out[string]           `api:"GET /escape/v1/assets"`
}

// Test_client_path_values_cannot_change_the_route calls a real server through
// the client and checks that whatever a path value contains, the request lands
// on the endpoint the client called and the handler sees the original value.
func Test_client_path_values_cannot_change_the_route(t *testing.T) {

	impl := escapeAPI{
		GetAsset: convAPI.NewOutP1(func(ctx convCtx.Context, id string) (string, error) {
			return "asset:" + id, nil
		}),
		GetProjection: convAPI.NewOutP1(func(ctx convCtx.Context, id string) (string, error) {
			return "projection:" + id, nil
		}),
		ListAssets: convAPI.NewOut(func(ctx convCtx.Context) (string, error) {
			return "list", nil
		}),
	}

	allowAll := func(r *http.Request) (convAuth.Target, error) { return convAuth.Target{}, nil }
	ctx := convCtx.New(convAuth.Claims{User: "escape-test"})

	srv := httptest.NewTLSServer(convAPI.NewHandler(ctx, "127.0.0.1", 0, allowAll, &impl))
	defer srv.Close()

	// The client sends through http.DefaultClient; trust the test server for
	// the duration of this test.
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	defer func() { http.DefaultClient.Transport = prev }()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	client := convAPI.NewClient[escapeAPI](u.Hostname(), port)

	ids := []string{
		"abc-123",
		"a/b",
		"x/@projection",
		"/",
		"%2F",
		"..%2F..",
		"a?b=c",
		"a#b",
		"a b",
		"müller",
	}

	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			got, err := client.GetAsset.Call(ctx, id)
			if err != nil {
				t.Fatalf("GetAsset.Call(%q) error: %v", id, err)
			}
			if want := "asset:" + id; got != want {
				t.Errorf("GetAsset.Call(%q) = %q, want %q", id, got, want)
			}
		})
	}
}
