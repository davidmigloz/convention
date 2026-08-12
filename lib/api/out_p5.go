package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func NewOutP5[outT any, p1T, p2T, p3T, p4T, p5T ~string](fn func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T, p5 p5T) (outT, error)) OutP5[outT, p1T, p2T, p3T, p4T, p5T] {
	return OutP5[outT, p1T, p2T, p3T, p4T, p5T]{
		fn: fn,
	}
}

func (x OutP5[outT, p1T, p2T, p3T, p4T, p5T]) WithPreCheck(check Check) OutP5[outT, p1T, p2T, p3T, p4T, p5T] {
	return OutP5[outT, p1T, p2T, p3T, p4T, p5T]{
		fn: func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T, p5 p5T) (res outT, err error) {
			err = check(ctx)
			if err != nil {
				return
			}
			return x.fn(ctx, p1, p2, p3, p4, p5)
		},
	}
}

func (x OutP5[outT, p1T, p2T, p3T, p4T, p5T]) WithPostCheck(check Check) OutP5[outT, p1T, p2T, p3T, p4T, p5T] {
	return OutP5[outT, p1T, p2T, p3T, p4T, p5T]{
		fn: func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T, p5 p5T) (res outT, err error) {
			res, err = x.fn(ctx, p1, p2, p3, p4, p5)
			if err != nil {
				return
			}
			err = check(ctx)
			if err != nil {
				return
			}
			return
		},
	}
}

type OutP5[outT any, p1T, p2T, p3T, p4T, p5T ~string] struct {
	descriptor descriptor
	fn         func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T, p5 p5T) (outT, error)
}

func (x *OutP5[outT, p1T, p2T, p3T, p4T, p5T]) execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool {

	values, match := x.descriptor.match(r)
	if !match {
		return false
	}

	out, err := x.fn(
		ctx,
		p1T(values.GetByIndex(0)),
		p2T(values.GetByIndex(1)),
		p3T(values.GetByIndex(2)),
		p4T(values.GetByIndex(3)),
		p5T(values.GetByIndex(4)),
	)
	if err != nil {
		var apiErr *Error
		if errors.As(err, &apiErr) {
			serveError(ctx, w, apiErr)
		} else {
			ServeError(ctx, w, http.StatusInternalServerError, ErrorCodeInternalError, "unexpected error", err)
		}
	} else {
		ServeJSON(w, out)
	}

	return true
}

func (x *OutP5[outT, p1T, p2T, p3T, p4T, p5T]) setDescriptor(desc descriptor) {
	x.descriptor = desc
}

func (x *OutP5[outT, p1T, p2T, p3T, p4T, p5T]) getDescriptor() descriptor {
	return x.descriptor
}

func (x *OutP5[outT, p1T, p2T, p3T, p4T, p5T]) getInOutTypes() (in, out reflect.Type) {
	return nil, reflect.TypeOf(new(outT))
}

func (x *OutP5[outT, p1T, p2T, p3T, p4T, p5T]) setEndpoints(eps endpoints) {}

func (x *OutP5[outT, p1T, p2T, p3T, p4T, p5T]) Call(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T, p5 p5T) (out outT, err error) {

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

	req, err := x.descriptor.newRequest(values, nil)
	if err != nil {
		return
	}

	err = setContextHttpHeaders(ctx, req)
	if err != nil {
		return
	}

	req.Header.Add("Accept", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}

	if res.StatusCode == http.StatusOK {
		err = json.NewDecoder(res.Body).Decode(&out)
		return
	}

	err = parseRemoteError(ctx, req, res)

	return
}
