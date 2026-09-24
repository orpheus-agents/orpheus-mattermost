// smoke-server supplies local dependencies for the production container check.
package main

import (
	"net/http"
	"time"
)

func main() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaa","username":"orpheus","is_bot":true}`))
		case "/api/v4/config/client":
			_, _ = w.Write([]byte(`{"MaxPostSize":"16383"}`))
		case "/api/v4/users/me/channels":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v1/sessions":
			_, _ = w.Write([]byte(`{"items":[],"next_cursor":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	server := &http.Server{Addr: ":8080", Handler: handler, ReadHeaderTimeout: time.Second}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}
