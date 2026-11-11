package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"

	convAuth "github.com/sofmon/convention/lib/auth"
	convCfg "github.com/sofmon/convention/lib/cfg"
	convCtx "github.com/sofmon/convention/lib/ctx"
)

type server struct {
	httpServer *http.Server
}

func NewServer(ctx convCtx.Context, host string, port int, policy convAuth.Policy, svc any) (srv *server, err error) {

	if port == 0 {
		port = 443
	}

	check, err := convAuth.NewCheck(policy)
	if err != nil {
		return
	}

	srv = &server{
		&http.Server{
			Addr:    fmt.Sprintf("%s:%d", host, port),
			Handler: NewHandler(ctx, host, port, check, svc),
		},
	}

	return
}

func (srv *server) EnableCallsLogging() {
	h, ok := srv.httpServer.Handler.(*httpHandler)
	if ok {
		h.logCalls = true
	}
}

func (srv *server) SkipDecodeClaims() {
	h, ok := srv.httpServer.Handler.(*httpHandler)
	if ok {
		h.skipDecodeClaims = true
	}
}

func (srv *server) ListenAndServe() (err error) {
	return srv.httpServer.ListenAndServeTLS(
		convCfg.FilePath("communication_certificate"), // following convention/v1
		convCfg.FilePath("communication_key"),         // following convention/v1
	)
}

func (srv *server) Shutdown(ctx convCtx.Context) (err error) {
	return srv.httpServer.Shutdown(ctx)
}

func NewHandler(ctx convCtx.Context, host string, port int, check convAuth.Check, svc any) http.Handler {
	return &httpHandler{ctx, computeEndpoints(host, port, svc), check, false, false}
}

func computeEndpoints(host string, port int, api any) (eps endpoints) {

	for _, f := range reflect.VisibleFields(reflect.TypeOf(api).Elem()) {

		ep, ok := reflect.ValueOf(api).Elem().FieldByName(f.Name).Addr().Interface().(endpoint)
		if !ok {
			continue
		}

		apiTag := f.Tag.Get("api")
		in, out := ep.getInOutTypes()
		desc := newDescriptor(host, port, apiTag, in, out)
		ep.setDescriptor(desc)

		eps = append(eps, ep)
	}

	sort.Slice(
		eps,
		func(i, j int) bool {
			return eps[i].getDescriptor().weight > eps[j].getDescriptor().weight
		},
	)

	for _, ep := range eps {
		ep.setEndpoints(eps)
	}

	return
}

type httpHandler struct {
	ctx              convCtx.Context
	eps              endpoints
	check            convAuth.Check
	logCalls         bool
	skipDecodeClaims bool
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := h.ctx.WithRequestStart().WithRequest(r, !h.skipDecodeClaims)

	_, err := h.check(r)
	if err != nil {
		switch err {
		case convAuth.ErrMissingRequest:
			ServeError(ctx, w, http.StatusBadRequest, ErrorCodeBadRequest, "missing http request", err)
			return
		case convAuth.ErrForbidden,
			convAuth.ErrMissingAuthorizationHeader,
			convAuth.ErrInvalidAuthorizationToken:
			ServeError(ctx, w, http.StatusForbidden, ErrorCodeForbidden, "missing or wrong authentication token", err)
			return
		default:
			ServeError(ctx, w, http.StatusUnauthorized, ErrorCodeUnauthorized, "unexpected error", err)
			return
		}
	}

	if h.logCalls {
		logCall(ctx, w, r, func(w http.ResponseWriter, r *http.Request) {
			execIfMatch(ctx, w, r, h.eps)
		})
	} else {
		execIfMatch(ctx, w, r, h.eps)
	}
}

func execIfMatch(ctx convCtx.Context, w http.ResponseWriter, r *http.Request, eps endpoints) {
	ok := false
	for _, ep := range eps {
		ok = ep.execIfMatch(ctx, w, r)
		if ok {
			break
		}
	}
	if !ok {
		ServeError(ctx, w, http.StatusNotFound, ErrorCodeNotFound, "Endpoint not found", nil)
	}
}

func logCall(ctx convCtx.Context, w http.ResponseWriter, r *http.Request, handle func(w http.ResponseWriter, r *http.Request)) {

	logger := ctx.Logger()
	if logger == nil {
		handle(w, r)
		return
	}

	// Check if request body should be logged
	reqContentType := r.Header.Get("Content-Type")
	reqHasBody := (r.ContentLength != 0 && r.Body != nil) || r.Header.Get("Transfer-Encoding") == "chunked"
	reqLogBody := reqHasBody && shouldLogBody(reqContentType)

	var reqBody []byte
	if reqLogBody {
		reqBody, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	// Copy headers and mask Authorization for logging
	reqHeaders := make(http.Header)
	for k, v := range r.Header {
		reqHeaders[k] = v
	}
	if authHeader := reqHeaders.Get(convAuth.HttpHeaderAuthorization); authHeader != "" {
		l := len(authHeader) - 10
		if l < 0 {
			l = len(authHeader)
		}
		reqHeaders.Set(convAuth.HttpHeaderAuthorization, "..."+authHeader[l:])
	}

	rec := httptest.NewRecorder()
	handle(rec, r)
	res := rec.Result()

	// Read response body (always needed to forward to client)
	var resBody []byte
	if res.Body != nil {
		resBody, _ = io.ReadAll(res.Body)
	}

	// Check if response body should be logged
	resContentType := res.Header.Get("Content-Type")
	resHasBody := len(resBody) > 0
	resLogBody := resHasBody && shouldLogBody(resContentType)

	// Write response to actual writer
	for k, v := range res.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}
	w.WriteHeader(res.StatusCode)
	if len(resBody) > 0 {
		_, err := w.Write(resBody)
		if err != nil {
			logger.Warn("error writing response body after logging", "error", err)
			return
		}
	}

	elapsed, hasElapsed := ctx.RequestElapsed()

	// Build log entry
	var reqBodyLog any
	if reqHasBody {
		if reqLogBody {
			reqBodyLog = processBody(reqContentType, reqBody)
		} else {
			reqBodyLog = "{{binary data}}"
		}
	}

	var resBodyLog any
	if resHasBody {
		if resLogBody {
			resBodyLog = processBody(resContentType, resBody)
		} else {
			resBodyLog = "{{binary data}}"
		}
	}

	attrs := []any{
		slog.Group("request",
			"method", r.Method,
			"url", r.URL.String(),
			slog.Group("headers", headersToAttrs(reqHeaders)...),
			"body", reqBodyLog,
		),
		slog.Group("response",
			"status", res.StatusCode,
			slog.Group("headers", headersToAttrs(res.Header)...),
			"body", resBodyLog,
		),
	}
	if hasElapsed {
		attrs = append(attrs, "duration_ms", elapsed.Milliseconds())
	}

	logger.With(attrs...).Info("API call")
}

func headersToAttrs(headers http.Header) []any {
	var attrs []any
	for name, values := range headers {
		attrs = append(attrs, name, strings.Join(values, ", "))
	}
	return attrs
}

func shouldLogBody(contentType string) bool {
	return strings.HasPrefix(contentType, "application/json") ||
		strings.HasPrefix(contentType, "application/xml") ||
		strings.HasPrefix(contentType, "application/yaml") ||
		strings.HasPrefix(contentType, "text/plain") ||
		strings.HasPrefix(contentType, "text/html")
}

func processBody(contentType string, body []byte) any {
	if len(body) == 0 {
		return nil
	}
	if strings.HasPrefix(contentType, "application/json") {
		var v any
		if json.Unmarshal(body, &v) == nil {
			return v
		}
		return string(body)
	}
	if strings.HasPrefix(contentType, "application/xml") ||
		strings.HasPrefix(contentType, "application/yaml") ||
		strings.HasPrefix(contentType, "text/plain") ||
		strings.HasPrefix(contentType, "text/html") {
		return string(body)
	}
	return "{{binary data}}"
}
