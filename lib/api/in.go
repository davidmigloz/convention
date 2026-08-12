package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func NewIn[inT any](fn func(ctx convCtx.Context, in inT) error) In[inT] {
	return In[inT]{
		fn: fn,
	}
}

func (x In[inT]) WithPreCheck(check Check) In[inT] {
	return In[inT]{
		fn: func(ctx convCtx.Context, in inT) error {
			err := check(ctx)
			if err != nil {
				return err
			}
			return x.fn(ctx, in)
		},
	}
}

func (x In[inT]) WithPostCheck(check Check) In[inT] {
	return In[inT]{
		fn: func(ctx convCtx.Context, in inT) error {
			err := x.fn(ctx, in)
			if err != nil {
				return err
			}
			return check(ctx)
		},
	}
}

type In[inT any] struct {
	descriptor descriptor
	fn         func(ctx convCtx.Context, in inT) error
}

func (x *In[inT]) execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool {

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

	err = x.fn(ctx, in)
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

func (x *In[inT]) setDescriptor(desc descriptor) {
	x.descriptor = desc
}

func (x *In[inT]) getDescriptor() descriptor {
	return x.descriptor
}

func (x *In[inT]) getInOutTypes() (in, out reflect.Type) {
	return reflect.TypeOf(new(inT)), nil
}

func (x *In[inT]) setEndpoints(eps endpoints) {}

func (x *In[inT]) Call(ctx convCtx.Context, in inT) (err error) {

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
		return
	}

	err = parseRemoteError(ctx, req, res)

	return
}
