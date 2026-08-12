package api

import (
	"errors"
	"net/http"
	"reflect"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func NewTrigger(fn func(ctx convCtx.Context) error) Trigger {
	return Trigger{
		fn: fn,
	}
}

func (x Trigger) WithPreCheck(check Check) Trigger {
	return Trigger{
		fn: func(ctx convCtx.Context) error {
			err := check(ctx)
			if err != nil {
				return err
			}
			return x.fn(ctx)
		},
	}
}

func (x Trigger) WithPostCheck(check Check) Trigger {
	return Trigger{
		fn: func(ctx convCtx.Context) error {
			err := x.fn(ctx)
			if err != nil {
				return err
			}
			return check(ctx)
		},
	}
}

type Trigger struct {
	descriptor descriptor
	fn         func(ctx convCtx.Context) error
}

func (x *Trigger) execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool {

	_, match := x.descriptor.match(r)
	if !match {
		return false
	}

	err := x.fn(ctx)
	if err != nil {
		var apiErr *Error
		if errors.As(err, &apiErr) {
			serveError(ctx, w, apiErr)
		} else {
			ServeError(ctx, w, http.StatusInternalServerError, ErrorCodeInternalError, "unexpected error", err)
		}
	} else {
		w.WriteHeader(http.StatusOK)
	}

	return true
}

func (x *Trigger) setDescriptor(desc descriptor) {
	x.descriptor = desc
}

func (x *Trigger) getDescriptor() descriptor {
	return x.descriptor
}

func (x *Trigger) getInOutTypes() (in, out reflect.Type) {
	return nil, nil
}

func (x *Trigger) setEndpoints(eps endpoints) {}

func (x *Trigger) Call(ctx convCtx.Context) (err error) {

	if !x.descriptor.isSet() {
		err = errors.New("api not initialized as client; user convAPI.NewClient to create client form api definition")
		return
	}

	req, err := x.descriptor.newRequest(nil, nil)
	if err != nil {
		return
	}

	err = setContextHttpHeaders(ctx, req)
	if err != nil {
		return
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}

	if res.StatusCode == http.StatusOK {
		return
	}

	err = parseRemoteError(ctx, req, res)

	return
}
