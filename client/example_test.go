package client_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/berkheev/tollgate/client"
)

func ExampleClient_Check() {
	tg := client.New("http://localhost:8090")

	d, err := tg.Check(context.Background(), "login_attempts", "203.0.113.7")
	if err != nil {
		log.Printf("tollgate unavailable: %v", err)
		return
	}
	if !d.Allowed {
		fmt.Printf("too many attempts, try again in %s\n", d.RetryAfter.Round(time.Second))
	}
}

func ExampleMiddleware() {
	tg := client.New("http://localhost:8090", client.WithTimeout(50*time.Millisecond))

	limit := client.Middleware(tg, "api_per_user", client.ByHeader("X-User-ID"),
		client.OnError(func(r *http.Request, err error) {
			log.Printf("rate limiter error on %s: %v", r.URL.Path, err)
		}),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /orders", limit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "[]")
	})))

	srv := &http.Server{Addr: ":8081", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
