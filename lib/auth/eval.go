package auth

import (
	"errors"
	"net/http"
	"sort"
	"strings"
)

var (
	ErrForbidden = errors.New("authenticated user has no permission to access the requested resource")
)

type allowedActions []allowedAction

type allowedAction struct {
	method  string
	path    allowedPath
	openEnd bool
}

// actionSource tracks which role an action came from
type actionSource struct {
	action allowedAction
	role   Role
}

type allowedActionSources []actionSource

func expandConfig(policy Policy) (actions allowedActionSources, publicActions allowedActions, err error) {

	actions = make(allowedActionSources, 0)

	for role, permissions := range policy.Roles {
		for _, permission := range permissions {
			as, ok := policy.Permissions[permission]
			if !ok {
				continue
			}

			for _, a := range as {
				var aa allowedAction
				aa, err = generateAllowedAction(a)
				if err != nil {
					return
				}
				actions = append(actions, actionSource{
					action: aa,
					role:   role,
				})
			}
		}
	}

	for _, a := range policy.Public {
		var aa allowedAction
		aa, err = generateAllowedAction(a)
		if err != nil {
			return
		}
		publicActions = append(publicActions, aa)
	}

	// Sort by specificity (most specific first)
	actions.sortBySpecificity()
	publicActions.sortBySpecificity()

	return
}

type Target struct {
	Tenant Tenant
	User   User
	Entity Entity
}

type Check func(r *http.Request) (Target, error)

func NewCheck(policy Policy) (check Check, err error) {

	actionSources, publicEndpoints, err := expandConfig(policy)
	if err != nil {
		return
	}

	check = func(r *http.Request) (Target, error) {

		var target Target

		if r == nil {
			return target, ErrMissingRequest
		}

		// Split the path exactly as the API router does, so the policy
		// evaluates the route that will run.
		segments, ok := PathSegments(r.URL)
		if !ok {
			return target, ErrForbidden
		}

		if publicEndpoints.match(
			r.Method,
			segments,
			Claims{},
			&target,
		) {
			return target, nil
		}

		claims, err := DecodeHTTPRequestClaims(r)
		if err != nil {
			return Target{}, err
		}

		if actionSources.match(
			r.Method,
			segments,
			claims,
			&target,
		) {
			return target, nil
		}

		return target, ErrForbidden
	}

	return
}

func generateAllowedAction(a Action) (res allowedAction, err error) {

	method, path, err := a.MethodPath()
	if err != nil {
		return
	}

	segments := strings.Split(path, "/")

	openEnd := strings.HasSuffix(path, "{any...}")
	if openEnd {
		segments = segments[:len(segments)-1]
	}

	allowedPath := make(allowedPath, len(segments))

	for i, segment := range segments {
		switch segment {
		case "{any}":
			allowedPath[i] = allowedSegmentAny{}
		case "{user}":
			allowedPath[i] = allowedSegmentUser{}
		case "{tenant}":
			allowedPath[i] = allowedSegmentTenant{}
		case "{entity}":
			allowedPath[i] = allowedSegmentEntity{}
		default:
			allowedPath[i] = allowedSegmentFixed(segment)
		}
	}

	res = allowedAction{method, allowedPath, openEnd}

	return
}

func (sources allowedActionSources) match(method string, segments []string, claims Claims, target *Target) bool {
	for _, src := range sources {
		// Try to match this action
		tempTarget := Target{}
		if !src.action.match(method, segments, claims, &tempTarget) {
			continue
		}

		// Action matched! Now validate the role is allowed for this context
		if isRoleAllowed(src.role, tempTarget.Entity, claims) {
			*target = tempTarget
			return true
		}
	}
	return false
}

// isRoleAllowed checks if the role is valid for the matched entity context
func isRoleAllowed(role Role, matchedEntity Entity, claims Claims) bool {
	// Check if role is in user's base roles
	for _, r := range claims.Roles {
		if r == role {
			return true
		}
	}

	// If no entity was matched in the path, only base roles apply
	if matchedEntity == "" {
		return false
	}

	// Check if role is in the matched entity's roles
	if entityRoles, ok := claims.Entities[matchedEntity]; ok {
		for _, r := range entityRoles {
			if r == role {
				return true
			}
		}
	}

	return false
}

func (as allowedActions) match(method string, segments []string, claims Claims, target *Target) bool {
	for _, allowedAction := range as {
		if allowedAction.match(method, segments, claims, target) {
			return true
		}
	}
	return false
}

func (a allowedAction) match(method string, segments []string, claims Claims, target *Target) bool {

	if a.method != method {
		return false
	}

	if a.openEnd {
		if len(a.path) > len(segments) {
			return false
		}
		return a.path.match(segments[:len(a.path)], claims, target)
	} else {
		return a.path.match(segments, claims, target)
	}
}

type allowedPath []allowedSegment

func (p allowedPath) match(segments []string, claims Claims, target *Target) bool {
	if len(p) != len(segments) {
		return false
	}
	for i := range p {
		// A policy segment matches one plain segment only. PathSegments keeps
		// an encoded '/' inside its segment, but an open-route handler reads
		// the decoded path, so "private%2Fdoc" must not pass as one {any}.
		if !isPlainSegment(segments[i]) {
			return false
		}
		if !p[i].Match(segments[i], claims, target) {
			return false
		}
	}
	return true
}

type allowedSegment interface {
	Match(segment string, claims Claims, target *Target) bool
}

type allowedSegmentFixed string

func (s allowedSegmentFixed) Match(segment string, claims Claims, target *Target) bool {
	return string(s) == segment
}

type allowedSegmentAny struct{}

func (s allowedSegmentAny) Match(segment string, claims Claims, target *Target) bool {
	return true
}

type allowedSegmentUser struct{}

func (s allowedSegmentUser) Match(segment string, claims Claims, target *Target) bool {
	if claims.User == User(segment) {
		target.User = User(segment)
		return true
	}
	return false
}

type allowedSegmentTenant struct{}

func (s allowedSegmentTenant) Match(segment string, claims Claims, target *Target) bool {
	tenant := Tenant(segment)
	for _, t := range claims.Tenants {
		if t == tenant {
			target.Tenant = tenant
			return true
		}
	}
	return false
}

type allowedSegmentEntity struct{}

func (s allowedSegmentEntity) Match(segment string, claims Claims, target *Target) bool {
	entity := Entity(segment)

	// Check if entity exists in the Entities map
	if _, exists := claims.Entities[entity]; exists {
		target.Entity = entity
		return true
	}

	return false
}

// segmentSpecificity returns a score for a segment type (lower = more specific)
func segmentSpecificity(seg allowedSegment) int {
	switch seg.(type) {
	case allowedSegmentFixed:
		return 0 // Most specific - exact match
	case allowedSegmentUser:
		return 1 // Matches only authenticated user
	case allowedSegmentTenant:
		return 2 // Matches any user tenant
	case allowedSegmentEntity:
		return 3 // Matches any user entity
	case allowedSegmentAny:
		return 4 // Matches anything
	default:
		return 5
	}
}

// actionSpecificity calculates total specificity for an action
// Returns a comparable value where lower = more specific
func actionSpecificity(a allowedAction) int {
	score := 0

	// Sum up segment specificities
	for _, seg := range a.path {
		score += segmentSpecificity(seg)
	}

	// openEnd ({any...}) is least specific - add penalty
	if a.openEnd {
		score += 1000 // Large penalty to ensure openEnd sorts last
	}

	// Longer paths are more specific (when scores are equal)
	// Subtract path length to prefer longer exact matches
	score -= len(a.path)

	return score
}

// sortBySpecificity sorts actions from most specific to least specific
func (sources allowedActionSources) sortBySpecificity() {
	sort.Slice(sources, func(i, j int) bool {
		return actionSpecificity(sources[i].action) < actionSpecificity(sources[j].action)
	})
}

func (actions allowedActions) sortBySpecificity() {
	sort.Slice(actions, func(i, j int) bool {
		return actionSpecificity(actions[i]) < actionSpecificity(actions[j])
	})
}
