package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

type ErrorCode string

const (
	ErrorCodeInternalError        ErrorCode = "internal_error"
	ErrorCodeNotFound             ErrorCode = "not_found"
	ErrorCodeBadRequest           ErrorCode = "bad_request"
	ErrorCodeForbidden            ErrorCode = "forbidden"
	ErrorCodeUnauthorized         ErrorCode = "unauthorized"
	ErrorCodeUnexpectedStatusCode ErrorCode = "unexpected_status_code"
)

func ErrorHasCode(err error, code ErrorCode) bool {
	if err == nil {
		return false
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == code
	}
	return false
}

func NewError(ctx convCtx.Context, status int, code ErrorCode, message string, inner error) error {
	return newError(ctx, status, code, message, inner)
}

func newError(ctx convCtx.Context, status int, code ErrorCode, message string, inner error) (err *Error) {

	err = &Error{
		Status:   status,
		Code:     code,
		Message:  message,
		Scope:    ctx.Scope(),
		Workflow: string(ctx.Workflow()),
	}

	r := ctx.Request()
	if r != nil {
		err.Method = r.Method
		err.URL = r.URL.Path
	}
	if inner != nil {
		switch apiErr := inner.(type) {
		case *Error:
			err.Inner = apiErr
		case Error:
			// value-typed convention errors must be absorbed as well, otherwise
			// their (already sanitised) content would be flattened into Message
			cp := apiErr
			err.Inner = &cp
		default:
			// anything else is an internal cause: kept for server-side logging
			// only, never rendered into the client visible Message
			err.detail = inner
		}
	}

	return
}

type Error struct {
	URL      string    `json:"url,omitempty"`
	Method   string    `json:"method,omitempty"`
	Status   int       `json:"status,omitempty"`
	Code     ErrorCode `json:"code,omitempty"`
	Scope    string    `json:"scope,omitempty"`
	Workflow string    `json:"workflow,omitempty"`
	Message  string    `json:"message,omitempty"`
	Inner    *Error    `json:"inner,omitempty"`

	// detail holds a non-convention error that caused this one (database
	// driver errors, scope wrapped errors, third party client errors, ...).
	// It is deliberately unexported so it can never be serialised towards a
	// client; it is only rendered by Error() for server side logging.
	detail error
}

func (e Error) Error() string {
	sb := strings.Builder{}
	sb.WriteString("✘ ")
	sb.WriteString(e.Method)
	sb.WriteRune(' ')
	sb.WriteString(e.URL)
	sb.WriteString(" → ")
	sb.WriteString(strconv.Itoa(e.Status))
	sb.WriteRune(' ')
	sb.WriteString(string(e.Code))
	sb.WriteString(" → ")
	sb.WriteString(e.Message)
	if e.detail != nil {
		sb.WriteString(" → ")
		sb.WriteString(e.detail.Error())
	}
	if e.Inner != nil {
		sb.WriteString(" → ")
		sb.WriteString(e.Inner.Error())
	}
	return sb.String()
}

// Unwrap exposes the causing error so errors.Is and errors.As can traverse the
// full chain, including non-convention causes such as convDB sentinel errors.
func (e Error) Unwrap() error {
	if e.Inner != nil {
		return e.Inner
	}
	return e.detail
}

// As allows errors.As to match value typed Errors onto a **Error target; without
// it a value typed Error would be skipped and the traversal would resolve to the
// deeper Inner error instead of this one.
func (e Error) As(target any) bool {
	t, ok := target.(**Error)
	if !ok {
		return false
	}
	cp := e
	*t = &cp
	return true
}

// publicView projects the parts of the error a client is allowed to see. The
// scope chain, the inner chain and the internal cause are all dropped: they
// carry internal call graphs, scope arguments and driver level detail. The
// workflow ID is retained so a sanitised response can still be correlated with
// the full server side log record.
func (e *Error) publicView() *Error {
	if e == nil {
		return nil
	}
	return &Error{
		URL:      e.URL,
		Method:   e.Method,
		Status:   e.Status,
		Code:     e.Code,
		Workflow: e.Workflow,
		Message:  e.Message,
	}
}

func ServeError(ctx convCtx.Context, w http.ResponseWriter, status int, code ErrorCode, message string, inner error) {
	serveError(ctx, w, newError(ctx, status, code, message, inner))
}

func serveError(ctx convCtx.Context, w http.ResponseWriter, err *Error) {

	// the response body is sanitised, so this log record is the only place the
	// full error chain is retained; it must be emitted at a level that is not
	// filtered out by the default handler
	logger := ctx.Logger()
	if logger != nil {
		if err.Status >= http.StatusInternalServerError {
			logger.Error("serving error response", "error", err.Error(), "status", err.Status, "code", err.Code)
		} else {
			logger.Warn("serving error response", "error", err.Error(), "status", err.Status, "code", err.Code)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(err.Status)
	json.NewEncoder(w).Encode(err.publicView())
}

func parseRemoteError(ctx convCtx.Context, req *http.Request, res *http.Response) (err error) {

	targetUrl := req.URL.Path
	targetMethod := req.Method

	var (
		inner *Error
	)
	inner = &Error{} // reserve memory for inner error
	if e := json.NewDecoder(res.Body).Decode(inner); e != nil {
		inner = nil // no inner error
	}

	if inner != nil &&
		(inner.URL == "" || inner.Method == "" || inner.Status == 0) { // inner is not complete
		inner = nil // no inner error
	}

	if inner != nil && inner.URL != targetUrl && inner.Method != targetMethod {
		// if URL and method matches return the inner error directly, no need to
		// wrap it again; it must stay a pointer so errors.As and ErrorHasCode
		// can match it on the calling side
		err = inner
		return
	}

	var code ErrorCode
	if inner != nil {
		code = inner.Code
	} else {
		code = ErrorCodeUnexpectedStatusCode
	}

	err = &Error{
		URL:     req.URL.Path,
		Method:  req.Method,
		Status:  res.StatusCode,
		Code:    code,
		Scope:   ctx.Scope(),
		Message: "unexpected status code: " + res.Status,
		Inner:   inner,
	}

	return
}
