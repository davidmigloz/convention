package api

import (
	"errors"
	"io"
	"net/http"
	"reflect"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func NewRawP1[p1T ~string](fn func(ctx convCtx.Context, p1 p1T, w http.ResponseWriter, r *http.Request)) RawP1[p1T] {
	return RawP1[p1T]{
		fn: fn,
	}
}

func (x RawP1[p1T]) WithPreCheck(check Check) RawP1[p1T] {
	return RawP1[p1T]{
		fn: func(ctx convCtx.Context, p1 p1T, w http.ResponseWriter, r *http.Request) {
			err := check(ctx)
			if err != nil {
				var apiErr *Error
				if errors.As(err, &apiErr) {
					serveError(ctx, w, apiErr)
				} else {
					ServeError(ctx, w, http.StatusInternalServerError, ErrorCodeInternalError, "unexpected error", err)
				}
				return
			}

			x.fn(ctx, p1, w, r)
		},
	}
}

type RawP1[p1T ~string] struct {
	descriptor descriptor
	fn         func(ctx convCtx.Context, p1 p1T, w http.ResponseWriter, r *http.Request)
}

func (x *RawP1[p1T]) execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool {

	values, match := x.descriptor.match(r)
	if !match {
		return false
	}

	x.fn(
		ctx,
		p1T(values.GetByIndex(0)),
		w,
		r,
	)

	return true
}

func (x *RawP1[p1T]) setDescriptor(desc descriptor) {
	x.descriptor = desc
}

func (x *RawP1[p1T]) getDescriptor() descriptor {
	return x.descriptor
}

func (x *RawP1[p1T]) getInOutTypes() (in, out reflect.Type) {
	return nil, nil
}

func (x *RawP1[p1T]) setEndpoints(eps endpoints) {}

func (x *RawP1[p1T]) Call(ctx convCtx.Context, p1 p1T, body io.Reader) (err error) {

	if !x.descriptor.isSet() {
		err = errors.New("api not initialized as client; user convAPI.NewClient to create client form api definition")
		return
	}

	values := values{
		{Name: "", Value: string(p1)},
	}

	req, err := x.descriptor.newRequest(values, body)
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
