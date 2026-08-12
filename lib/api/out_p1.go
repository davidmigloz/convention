package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func NewOutP1[outT any, p1T ~string](fn func(ctx convCtx.Context, p1 p1T) (outT, error)) OutP1[outT, p1T] {
	return OutP1[outT, p1T]{
		fn: fn,
	}
}

func (x OutP1[outT, p1T]) WithPreCheck(check Check) OutP1[outT, p1T] {
	return OutP1[outT, p1T]{
		fn: func(ctx convCtx.Context, p1 p1T) (res outT, err error) {
			err = check(ctx)
			if err != nil {
				return
			}
			return x.fn(ctx, p1)
		},
	}
}

func (x OutP1[outT, p1T]) WithPostCheck(check Check) OutP1[outT, p1T] {
	return OutP1[outT, p1T]{
		fn: func(ctx convCtx.Context, p1 p1T) (res outT, err error) {
			res, err = x.fn(ctx, p1)
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

type OutP1[outT any, p1T ~string] struct {
	descriptor descriptor
	fn         func(ctx convCtx.Context, p1 p1T) (outT, error)
}

func (x *OutP1[outT, p1T]) execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool {

	values, match := x.descriptor.match(r)
	if !match {
		return false
	}

	out, err := x.fn(
		ctx,
		p1T(values.GetByIndex(0)),
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

func (x *OutP1[outT, p1T]) setDescriptor(desc descriptor) {
	x.descriptor = desc
}

func (x *OutP1[outT, p1T]) getDescriptor() descriptor {
	return x.descriptor
}

func (x *OutP1[outT, p1T]) getInOutTypes() (in, out reflect.Type) {
	return nil, reflect.TypeOf(new(outT))
}

func (x *OutP1[outT, p1T]) setEndpoints(eps endpoints) {}

func (x *OutP1[outT, p1T]) Call(ctx convCtx.Context, p1 p1T) (out outT, err error) {

	if !x.descriptor.isSet() {
		err = errors.New("api not initialized as client; user convAPI.NewClient to create client form api definition")
		return
	}

	values := values{
		{Name: "", Value: string(p1)},
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
