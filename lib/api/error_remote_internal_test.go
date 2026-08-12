package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
)

// Exercises the full service-to-service relay: service B serves an error caused
// by a database failure, service A parses that response and then serves its own
// error to the end client. Nothing from B's internals may reach A's client.
func Test_parseRemoteError_relayDoesNotLeak(t *testing.T) {

	const (
		dbErrText = `pq: syntax error at or near "SELCT"`
		piiArg    = "ceo@acme.example"
	)

	// --- service B: fails on a bare returned database error -----------------

	reqB := httptest.NewRequest(http.MethodGet, "https://svc-b/pro-partner/x", nil)
	ctxB := convCtx.New(convAuth.Claims{User: "svc-b"}).WithRequest(reqB, false)
	ctxB = ctxB.WithScope("queryPartner", "email", piiArg)

	dbErr := errors.New(dbErrText)

	wB := httptest.NewRecorder()
	ServeError(ctxB, wB, http.StatusInternalServerError,
		ErrorCodeInternalError, "unexpected error", dbErr)

	bodyB := wB.Body.String()
	for _, leaked := range []string{dbErrText, piiArg, "queryPartner"} {
		if strings.Contains(bodyB, leaked) {
			t.Fatalf("service B response leaked %q\nbody: %s", leaked, bodyB)
		}
	}

	// --- service A: parses B's response -------------------------------------

	ctxA := convCtx.New(convAuth.Claims{User: "svc-a"})
	resB := &http.Response{
		StatusCode: wB.Code,
		Status:     "500 Internal Server Error",
		Body:       io.NopCloser(bytes.NewReader(wB.Body.Bytes())),
	}

	remoteErr := parseRemoteError(ctxA, reqB, resB)
	if remoteErr == nil {
		t.Fatal("expected an error from parseRemoteError")
	}

	// it must be a pointer, otherwise errors.As in every handler misses it
	if _, ok := remoteErr.(*Error); !ok {
		t.Fatalf("expected *Error from parseRemoteError, got %T", remoteErr)
	}

	// the remote code must survive, so callers can still branch on it
	if !ErrorHasCode(remoteErr, ErrorCodeInternalError) {
		t.Error("ErrorHasCode failed on the parsed remote error")
	}

	var apiErr *Error
	if !errors.As(remoteErr, &apiErr) {
		t.Fatal("errors.As failed on the parsed remote error")
	}

	// --- service A serves its own error to the end client -------------------

	reqA := httptest.NewRequest(http.MethodGet, "https://svc-a/partner/x", nil)
	ctxA = ctxA.WithRequest(reqA, false)

	wA := httptest.NewRecorder()
	ServeError(ctxA, wA, http.StatusInternalServerError,
		ErrorCodeInternalError, "unexpected error", remoteErr)

	bodyA := wA.Body.String()
	for _, leaked := range []string{dbErrText, piiArg, "queryPartner", "svc-b", "inner", "scope"} {
		if strings.Contains(bodyA, leaked) {
			t.Errorf("service A response leaked %q from the relay\nbody: %s", leaked, bodyA)
		}
	}

	var got Error
	if err := json.Unmarshal(wA.Body.Bytes(), &got); err != nil {
		t.Fatalf("service A response is not a valid api error: %v", err)
	}
	if got.Code != ErrorCodeInternalError {
		t.Errorf("expected code %q, got %q", ErrorCodeInternalError, got.Code)
	}
	if got.Workflow == "" {
		t.Error("expected a workflow ID for log correlation")
	}
}
