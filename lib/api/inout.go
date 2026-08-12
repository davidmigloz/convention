package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func NewInOut[inT, outT any](fn func(ctx convCtx.Context, in inT) (outT, error)) InOut[inT, outT] {
	return InOut[inT, outT]{
		fn: fn,
	}
}

func (x InOut[inT, outT]) WithPreCheck(check Check) InOut[inT, outT] {
	return InOut[inT, outT]{
		fn: func(ctx convCtx.Context, in inT) (res outT, err error) {
			err = check(ctx)
			if err != nil {
				return
			}
			return x.fn(ctx, in)
		},
	}
}

func (x InOut[inT, outT]) WithPostCheck(check Check) InOut[inT, outT] {
	return InOut[inT, outT]{
		fn: func(ctx convCtx.Context, in inT) (res outT, err error) {
			res, err = x.fn(ctx, in)
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

type InOut[inT, outT any] struct {
	descriptor descriptor
	fn         func(ctx convCtx.Context, in inT) (outT, error)
}

func (x *InOut[inT, outT]) execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool {

	_, match := x.descriptor.match(r)
	if !match {
		return false
	}

	var in inT
	err := json.NewDecoder(r.Body).Decode(&in)
	if err != nil {
		ServeError(ctx, w, http.StatusBadRequest, ErrorCodeBadRequest, "unable to decode http payload", err)
		return true
	}

	out, err := x.fn(ctx, in)
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

func (x *InOut[inT, outT]) setDescriptor(desc descriptor) {
	x.descriptor = desc
}

func (x *InOut[inT, outT]) getDescriptor() descriptor {
	return x.descriptor
}

func (x *InOut[inT, outT]) getInOutTypes() (in, out reflect.Type) {
	return reflect.TypeOf(new(inT)), reflect.TypeOf(new(outT))
}

func (x *InOut[inT, outT]) setEndpoints(eps endpoints) {}

func (x *InOut[inT, outT]) Call(ctx convCtx.Context, in inT) (out outT, err error) {

	if !x.descriptor.isSet() {
		err = errors.New("api not initialized as client; user convAPI.NewClient to create client form api definition")
		return
	}

	body, err := json.Marshal(in)
	if err != nil {
		return
	}

	req, err := x.descriptor.newRequest(nil, bytes.NewReader(body))
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
