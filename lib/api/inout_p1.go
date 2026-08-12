package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func NewInOutP1[inT, outT any, p1T ~string](fn func(ctx convCtx.Context, p1 p1T, in inT) (outT, error)) InOutP1[inT, outT, p1T] {
	return InOutP1[inT, outT, p1T]{
		fn: fn,
	}
}

func (x InOutP1[inT, outT, p1T]) WithPreCheck(check Check) InOutP1[inT, outT, p1T] {
	return InOutP1[inT, outT, p1T]{
		fn: func(ctx convCtx.Context, p1 p1T, in inT) (res outT, err error) {
			err = check(ctx)
			if err != nil {
				return
			}
			return x.fn(ctx, p1, in)
		},
	}
}

func (x InOutP1[inT, outT, p1T]) WithPostCheck(check Check) InOutP1[inT, outT, p1T] {
	return InOutP1[inT, outT, p1T]{
		fn: func(ctx convCtx.Context, p1 p1T, in inT) (res outT, err error) {
			res, err = x.fn(ctx, p1, in)
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

type InOutP1[inT, outT any, p1T ~string] struct {
	descriptor descriptor
	fn         func(ctx convCtx.Context, p1 p1T, in inT) (outT, error)
}

func (x *InOutP1[inT, outT, p1T]) execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool {

	values, match := x.descriptor.match(r)
	if !match {
		return false
	}

	var in inT
	err := json.NewDecoder(r.Body).Decode(&in)
	if err != nil {
		ServeError(ctx, w, http.StatusBadRequest, ErrorCodeBadRequest, "unable to decode http payload", err)
		return true
	}

	out, err := x.fn(
		ctx,
		p1T(values.GetByIndex(0)),
		in,
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

func (x *InOutP1[inT, outT, p1T]) setDescriptor(desc descriptor) {
	x.descriptor = desc
}

func (x *InOutP1[inT, outT, p1T]) getDescriptor() descriptor {
	return x.descriptor
}

func (x *InOutP1[inT, outT, p1T]) getInOutTypes() (in, out reflect.Type) {
	return reflect.TypeOf(new(inT)), reflect.TypeOf(new(outT))
}

func (x *InOutP1[inT, outT, p1T]) setEndpoints(eps endpoints) {}

func (x *InOutP1[inT, outT, p1T]) Call(ctx convCtx.Context, p1 p1T, in inT) (out outT, err error) {

	if !x.descriptor.isSet() {
		err = errors.New("api not initialized as client; user convAPI.NewClient to create client form api definition")
		return
	}

	body, err := json.Marshal(in)
	if err != nil {
		return
	}

	values := values{
		{Name: "", Value: string(p1)},
	}

	req, err := x.descriptor.newRequest(values, bytes.NewReader(body))
	if err != nil {
		return
	}

	err = setContextHttpHeaders(ctx, req)
	if err != nil {
		return
	}

	req.Header.Add("Content-Type", "application/json")
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
