package auth

import (
	"net/url"
	"strings"
)

// PathSegments splits a request path into its unescaped segments.
//
// It splits the path as it arrived on the wire and unescapes each segment
// afterwards, so an encoded '/' (%2F) stays inside its segment instead of
// becoming a separator. The wire form is u.RawPath when that is a valid
// encoding of u.Path, otherwise u.EscapedPath().
//
// The authorization check (NewCheck) and the convention API router both use
// this function, so a policy always evaluates the route that actually runs.
// ok is false when u is nil or a segment is not a valid escape sequence.
func PathSegments(u *url.URL) (segments []string, ok bool) {

	if u == nil {
		return nil, false
	}

	raw := u.EscapedPath()
	if u.RawPath != "" {
		if p, err := url.PathUnescape(u.RawPath); err == nil && p == u.Path {
			raw = u.RawPath
		}
	}

	segments = strings.Split(strings.Trim(raw, "/"), "/")
	for i, s := range segments {
		unescaped, err := url.PathUnescape(s)
		if err != nil {
			return nil, false
		}
		segments[i] = unescaped
	}

	return segments, true
}
