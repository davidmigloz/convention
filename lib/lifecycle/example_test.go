package lifecycle_test

import (
	"time"

	convAPI "github.com/sofmon/convention/lib/api"
	convAuth "github.com/sofmon/convention/lib/auth"
	convCtx "github.com/sofmon/convention/lib/ctx"
	convLifecycle "github.com/sofmon/convention/lib/lifecycle"
)

func ExampleRun() {
	ctx := convCtx.New(convAuth.Claims{User: "example-service"})
	server, err := convAPI.NewServer(ctx, "localhost", 8443, convAuth.Policy{}, &struct{}{})
	if err != nil {
		return
	}

	_ = convLifecycle.Run(ctx, convLifecycle.Config{
		ListenAndServe: func(convCtx.Context) error {
			return server.ListenAndServe()
		},
		ShutdownTimeout: 20 * time.Second,
		Stages: [][]convLifecycle.Stage{
			{
				{Name: "drain http server", Fn: server.Shutdown},
			},
		},
	})
}
