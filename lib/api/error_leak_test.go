package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	convAPI "github.com/sofmon/convention/lib/api"
	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
)

const (
	dbErrText   = `pq: column "pro_partner"."vat_number" does not exist`
	scopeArgPII = "ceo@acme.example"
)

// testCtx returns a context whose logger writes into the returned buffer, so a
// test can assert on what was recorded server-side.
func testCtx(t *testing.T) (convCtx.Context, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	ctx := convCtx.New(convAuth.Claims{User: "user-1234"}).
		WithLogger(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return ctx, buf
}

// loggedErrors returns the decoded "error" field of every JSON log record in
// buf, so assertions are not defeated by slog's JSON escaping.
func loggedErrors(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	sb := strings.Builder{}
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
		}
		for _, k := range []string{"error", "workflow"} {
			if v, ok := rec[k].(string); ok {
				sb.WriteString(v)
				sb.WriteRune('\n')
			}
		}
	}
	return sb.String()
}

// dbErrorThroughScopes mimics a bare-returned convDB error travelling up through
// convCtx.Exit frames, which is how it reaches a handler in practice.
func dbErrorThroughScopes(ctx convCtx.Context) error {
	err := errors.New(dbErrText)
	for _, f := range []struct {
		scope string
		args  []any
	}{
		{"selectProPartner", []any{"id", "0f9a-partner", "email", scopeArgPII}},
		{"getProPartner", []any{"tenant", "acme"}},
	} {
		fctx := ctx.WithScope(f.scope, f.args...)
		err = fmt.Errorf("%s: %w", "✘ "+fctx.Scope(), err)
	}
	return err
}

// A bare-returned database error must not reach the client in any form.
func Test_error_databaseDetailNotServed(t *testing.T) {

	ctx, logBuf := testCtx(t)

	w := httptest.NewRecorder()
	convAPI.ServeError(ctx, w, 500, convAPI.ErrorCodeInternalError,
		"unexpected error", dbErrorThroughScopes(ctx))

	body := w.Body.String()

	for _, leaked := range []string{
		dbErrText,          // postgres driver text, table and column names
		"pq:",              // driver prefix
		scopeArgPII,        // scope argument carrying PII
		"selectProPartner", // internal call graph
		"getProPartner",
		"0f9a-partner",
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("response body leaked %q\nbody: %s", leaked, body)
		}
	}

	// the client still gets a usable outer error
	var got convAPI.Error
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a valid api error: %v", err)
	}
	if got.Code != convAPI.ErrorCodeInternalError {
		t.Errorf("expected code %q, got %q", convAPI.ErrorCodeInternalError, got.Code)
	}
	if got.Message != "unexpected error" {
		t.Errorf("expected message %q, got %q", "unexpected error", got.Message)
	}

	// ...and the full chain is retained server side, which is now the only copy
	logged := loggedErrors(t, logBuf)
	for _, want := range []string{dbErrText, "selectProPartner", scopeArgPII} {
		if !strings.Contains(logged, want) {
			t.Errorf("server side log is missing %q\nlog: %s", want, logged)
		}
	}
}

// The inner chain must never be serialised, even when every frame is a
// convention error and no raw cause was ever passed as inner.
func Test_error_innerChainNotServed(t *testing.T) {

	ctx, _ := testCtx(t)

	deep := convAPI.NewError(ctx, 500, convAPI.ErrorCodeInternalError, "db read failed",
		errors.New(`pq: relation "internal_billing_ledger" does not exist`))
	mid := convAPI.NewError(ctx, 500, convAPI.ErrorCodeInternalError, "load partner", deep)
	top := convAPI.NewError(ctx, 400, convAPI.ErrorCodeBadRequest, "invalid partner", mid)

	w := httptest.NewRecorder()
	var apiErr *convAPI.Error
	if !errors.As(top, &apiErr) {
		t.Fatal("errors.As failed to match the api error")
	}
	convAPI.ServeError(ctx, w, apiErr.Status, apiErr.Code, apiErr.Message, apiErr)

	body := w.Body.String()
	for _, leaked := range []string{"inner", "internal_billing_ledger", "db read failed", "load partner", "scope"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response body leaked %q\nbody: %s", leaked, body)
		}
	}
}

// The workflow ID is retained so a sanitised response can be correlated with the
// full server side log record.
func Test_error_workflowIDServedForCorrelation(t *testing.T) {

	ctx, logBuf := testCtx(t)

	w := httptest.NewRecorder()
	convAPI.ServeError(ctx, w, 500, convAPI.ErrorCodeInternalError,
		"unexpected error", dbErrorThroughScopes(ctx))

	var got convAPI.Error
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a valid api error: %v", err)
	}

	if got.Workflow == "" {
		t.Fatal("expected workflow ID in the client visible error")
	}
	if got.Workflow != string(ctx.Workflow()) {
		t.Errorf("expected workflow %q, got %q", ctx.Workflow(), got.Workflow)
	}
	if !strings.Contains(loggedErrors(t, logBuf), got.Workflow) {
		t.Error("workflow ID served to the client is not present in the server log")
	}
}

// Error() must still render the full chain for server side logging.
func Test_error_stringRetainsFullChain(t *testing.T) {

	ctx, _ := testCtx(t)

	inner := convAPI.NewError(ctx, 500, convAPI.ErrorCodeInternalError, "db read failed",
		errors.New(dbErrText))
	outer := convAPI.NewError(ctx, 400, convAPI.ErrorCodeBadRequest, "invalid partner", inner)

	s := outer.Error()
	for _, want := range []string{"invalid partner", "db read failed", dbErrText} {
		if !strings.Contains(s, want) {
			t.Errorf("Error() is missing %q, got: %s", want, s)
		}
	}
}

// errors.Is / errors.As must traverse into the retained cause so services can
// still branch on sentinel errors such as convDB.ErrObjectNotFound.
func Test_error_unwrapReachesCause(t *testing.T) {

	ctx, _ := testCtx(t)

	sentinel := errors.New("object not found")
	wrapped := fmt.Errorf("select failed: %w", sentinel)

	apiErr := convAPI.NewError(ctx, 404, convAPI.ErrorCodeNotFound, "partner not found", wrapped)

	if !errors.Is(apiErr, sentinel) {
		t.Error("errors.Is failed to reach the retained cause")
	}

	// and through a nested convention chain
	outer := convAPI.NewError(ctx, 500, convAPI.ErrorCodeInternalError, "outer", apiErr)
	if !errors.Is(outer, sentinel) {
		t.Error("errors.Is failed to reach the cause through a nested api error")
	}
}

// A value typed Error (as produced by remote error parsing) must be matched by
// errors.As and by ErrorHasCode, otherwise handlers fall through to the branch
// that flattens it into a client visible Message.
func Test_error_valueTypedMatchedByAs(t *testing.T) {

	remote := convAPI.Error{
		URL: "/pro-partner/x", Method: "GET", Status: 500,
		Code:    convAPI.ErrorCodeInternalError,
		Message: "unexpected error",
	}

	var target *convAPI.Error
	if !errors.As(error(remote), &target) {
		t.Fatal("errors.As failed to match a value typed Error")
	}
	if target.Code != convAPI.ErrorCodeInternalError {
		t.Errorf("errors.As resolved to the wrong error: %+v", target)
	}

	if !convAPI.ErrorHasCode(error(remote), convAPI.ErrorCodeInternalError) {
		t.Error("ErrorHasCode failed for a value typed Error")
	}
}

// A value typed Error passed as inner must be absorbed as a structured inner
// error rather than flattened into the client visible Message.
func Test_error_valueTypedInnerAbsorbed(t *testing.T) {

	ctx, _ := testCtx(t)

	remote := convAPI.Error{
		Status: 500, Code: convAPI.ErrorCodeInternalError,
		Message: `unexpected error → ✘ svc-b → q {email=` + scopeArgPII + `}: ` + dbErrText,
	}

	w := httptest.NewRecorder()
	convAPI.ServeError(ctx, w, 500, convAPI.ErrorCodeInternalError, "unexpected error", error(remote))

	body := w.Body.String()
	for _, leaked := range []string{dbErrText, scopeArgPII, "svc-b"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response body leaked %q from a relayed remote error\nbody: %s", leaked, body)
		}
	}
}
