package api

import (
	"errors"
	"io"
	"net/http"
	"reflect"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func NewRawP5[p1T, p2T, p3T, p4T, p5T ~string](fn func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T, p5 p5T, w http.ResponseWriter, r *http.Request)) RawP5[p1T, p2T, p3T, p4T, p5T] {
	return RawP5[p1T, p2T, p3T, p4T, p5T]{
		fn: fn,
	}
}

func (x RawP5[p1T, p2T, p3T, p4T, p5T]) WithPreCheck(check Check) RawP5[p1T, p2T, p3T, p4T, p5T] {
	return RawP5[p1T, p2T, p3T, p4T, p5T]{
		fn: func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T, p5 p5T, w http.ResponseWriter, r *http.Request) {
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

			x.fn(ctx, p1, p2, p3, p4, p5, w, r)
		},
	}
}

type RawP5[p1T, p2T, p3T, p4T, p5T ~string] struct {
	descriptor descriptor
	fn         func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T, p5 p5T, w http.ResponseWriter, r *http.Request)
}

func (x *RawP5[p1T, p2T, p3T, p4T, p5T]) execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool {

	values, match := x.descriptor.match(r)
	if !match {
		return false
	}

	x.fn(
		ctx,
		p1T(values.GetByIndex(0)),
		p2T(values.GetByIndex(1)),
		p3T(values.GetByIndex(2)),
		p4T(values.GetByIndex(3)),
		p5T(values.GetByIndex(4)),
		w,
		r,
	)

	return true
}

func (x *RawP5[p1T, p2T, p3T, p4T, p5T]) setDescriptor(desc descriptor) {
	x.descriptor = desc
}

func (x *RawP5[p1T, p2T, p3T, p4T, p5T]) getDescriptor() descriptor {
	return x.descriptor
}

func (x *RawP5[p1T, p2T, p3T, p4T, p5T]) getInOutTypes() (in, out reflect.Type) {
	return nil, nil
}

func (x *RawP5[p1T, p2T, p3T, p4T, p5T]) setEndpoints(eps endpoints) {}

func (x *RawP5[p1T, p2T, p3T, p4T, p5T]) Call(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T, p5 p5T, body io.Reader) (err error) {

	if !x.descriptor.isSet() {
		err = errors.New("api not initialized as client; user convAPI.NewClient to create client form api definition")
		return
	}

	values := values{
		{Name: "", Value: string(p1)},
		{Name: "", Value: string(p2)},
		{Name: "", Value: string(p3)},
		{Name: "", Value: string(p4)},
		{Name: "", Value: string(p5)},
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
