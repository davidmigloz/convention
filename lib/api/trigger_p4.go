package api

import (
	"errors"
	"net/http"
	"reflect"

	convCtx "github.com/sofmon/convention/lib/ctx"
)

func NewTriggerP4[p1T, p2T, p3T, p4T ~string](fn func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T) error) TriggerP4[p1T, p2T, p3T, p4T] {
	return TriggerP4[p1T, p2T, p3T, p4T]{
		fn: fn,
	}
}

func (x TriggerP4[p1T, p2T, p3T, p4T]) WithPreCheck(check Check) TriggerP4[p1T, p2T, p3T, p4T] {
	return TriggerP4[p1T, p2T, p3T, p4T]{
		fn: func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T) error {
			err := check(ctx)
			if err != nil {
				return err
			}
			return x.fn(ctx, p1, p2, p3, p4)
		},
	}
}

func (x TriggerP4[p1T, p2T, p3T, p4T]) WithPostCheck(check Check) TriggerP4[p1T, p2T, p3T, p4T] {
	return TriggerP4[p1T, p2T, p3T, p4T]{
		fn: func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T) error {
			err := x.fn(ctx, p1, p2, p3, p4)
			if err != nil {
				return err
			}
			return check(ctx)
		},
	}
}

type TriggerP4[p1T, p2T, p3T, p4T ~string] struct {
	descriptor descriptor
	fn         func(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T) error
}

func (x *TriggerP4[p1T, p2T, p3T, p4T]) execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request) bool {

	values, match := x.descriptor.match(r)
	if !match {
		return false
	}

	err := x.fn(
		ctx,
		p1T(values.GetByIndex(0)),
		p2T(values.GetByIndex(1)),
		p3T(values.GetByIndex(2)),
		p4T(values.GetByIndex(3)),
	)
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

func (x *TriggerP4[p1T, p2T, p3T, p4T]) setDescriptor(desc descriptor) {
	x.descriptor = desc
}

func (x *TriggerP4[p1T, p2T, p3T, p4T]) getDescriptor() descriptor {
	return x.descriptor
}

func (x *TriggerP4[p1T, p2T, p3T, p4T]) getInOutTypes() (in, out reflect.Type) {
	return nil, nil
}

func (x *TriggerP4[p1T, p2T, p3T, p4T]) setEndpoints(eps endpoints) {}

func (x *TriggerP4[p1T, p2T, p3T, p4T]) Call(ctx convCtx.Context, p1 p1T, p2 p2T, p3 p3T, p4 p4T) (err error) {

	if !x.descriptor.isSet() {
		err = errors.New("api not initialized as client; user convAPI.NewClient to create client form api definition")
		return
	}

	values := values{
		{Name: "", Value: string(p1)},
		{Name: "", Value: string(p2)},
		{Name: "", Value: string(p3)},
		{Name: "", Value: string(p4)},
	}

	req, err := x.descriptor.newRequest(values, nil)
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
