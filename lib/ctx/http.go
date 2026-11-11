package ctx

import (
	"context"
	"fmt"
	"net/http"
	"time"

	convAuth "github.com/sofmon/convention/lib/auth"
)

type Workflow string

const (
	HttpHeaderAuthorization = convAuth.HttpHeaderAuthorization
	HttpHeaderWorkflow      = "Workflow"
	HTTPHeaderTimeNow       = "Time-Now"
)

func (ctx Context) WithRequest(r *http.Request, decodeClaims bool) (res Context) {

	res = Context{
		context.WithValue(
			ctx.Context,
			contextKeyRequest,
			r,
		),
	}

	if wid := r.Header.Get(HttpHeaderWorkflow); wid != "" {
		res = res.WithWorkflow(Workflow(wid))
	}

	action := convAuth.Action(fmt.Sprintf("%s %s", r.Method, r.URL.Path))
	res = res.WithAction(action)

	if decodeClaims {
		if claims, err := convAuth.DecodeHTTPRequestClaims(r); err == nil {
			res = res.WithClaims(claims)
		} else {
			if err != convAuth.ErrMissingAuthorizationHeader {
				res.Logger().Warn("failed to decode HTTP request claims", "error", err.Error())
			}
			// empty the claims to ensure that the context
			// would not provide the agent claims without
			// explicit ctx.WithAgentClaims() call
			res = res.WithClaims(convAuth.Claims{})
		}
	}

	if !ctx.IsProdEnv() && r != nil {
		nowStr := r.Header.Get(HTTPHeaderTimeNow)
		if nowStr != "" {
			now, err := time.Parse(time.RFC3339, nowStr)
			if err != nil {
				ctx.Logger().Warn("failed to parse '"+HTTPHeaderTimeNow+"' header", "error", err.Error())
			} else {
				res = res.WithNow(now.UTC())
			}
		}
	}

	return
}

// requestStamp pairs the arrival time with the clock that produced it, so the
// elapsed time is read on the same clock even after WithNow or a Time-Now
// header re-clocks a derived context.
type requestStamp struct {
	start time.Time
	now   func() time.Time
}

// WithRequestStart records that a request has just been received. A server
// calls it once, before anything else, on the context it builds the request
// context from; WithRequest does not stamp, so attaching another request to a
// derived context never moves the arrival time.
func (ctx Context) WithRequestStart() Context {
	return Context{
		context.WithValue(
			ctx.Context,
			contextKeyRequestStart,
			requestStamp{start: ctx.Now(), now: ctx.Now},
		),
	}
}

// RequestElapsed returns the time since WithRequestStart, read on the clock
// the stamped context had, so a request's Time-Now header does not change it.
// ok is false when the context was never stamped. Like Now it is wall-clock
// arithmetic, so a clock step backwards can give a negative duration.
func (ctx Context) RequestElapsed() (d time.Duration, ok bool) {
	stamp, ok := ctx.Value(contextKeyRequestStart).(requestStamp)
	if !ok {
		return 0, false
	}
	return stamp.now().Sub(stamp.start), true
}

func (ctx Context) Request() (r *http.Request) {
	obj := ctx.Value(contextKeyRequest)
	if obj == nil {
		return
	}
	return obj.(*http.Request)
}
