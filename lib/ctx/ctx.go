package ctx

import (
	"context"

	convAuth "github.com/sofmon/convention/lib/auth"

	"github.com/google/uuid"
)

type Context struct {
	context.Context
}

type contextKey int

const (
	contextKeyAgent contextKey = iota
	contextKeyAgentClaims
	contextKeyUseAgentClaims
	contextKeyEnv
	contextKeyRequest
	contextKeyClaims
	contextKeyAction
	contextKeyWorkflow
	contextKeyScope
	contextKeyNow
	contextKeyLogger
	contextKeyRequestStart

	loggerKeyEnv            = "env"
	loggerKeyAgent          = "agent"
	loggerKeyWorkflow       = "workflow"
	loggerKeyUser           = "user"
	loggerKeyScope          = "scope"
	loggerKeyAction         = "action"
	loggerKeyNow            = "now"
	loggerKeyUseAgentClaims = "use_agent_claims"
)

type Agent string

func New(claims convAuth.Claims) (ctx Context) {
	return WrapContext(context.Background(), claims)
}

func WrapContext(parent context.Context, claims convAuth.Claims) (ctx Context) {

	agent := Agent(claims.User)            // use the user as the agent name
	env := getEnv()                        // determine the environment
	workflow := Workflow(uuid.NewString()) // generate a new workflow ID
	scope := string(agent)                 // initial scope is the agent name

	ctx.Context = context.WithValue(parent, contextKeyEnv, env)
	ctx.Context = context.WithValue(ctx.Context, contextKeyAgent, agent)
	ctx.Context = context.WithValue(ctx.Context, contextKeyAgentClaims, claims)
	ctx.Context = context.WithValue(ctx.Context, contextKeyClaims, claims)
	ctx.Context = context.WithValue(ctx.Context, contextKeyWorkflow, workflow)
	ctx.Context = context.WithValue(ctx.Context, contextKeyScope, scope)
	// NOTE: No logger pre-built here - Logger() builds it lazily from context values

	return
}

func (ctx Context) Agent() Agent {
	obj := ctx.Value(contextKeyAgent)
	if obj == nil {
		return ""
	}
	return obj.(Agent)
}
