package main

import (
	"net/http"
	"net/http/pprof"
)

// registerPprof mounts the profiling endpoints on the admin mux.
//
// They live behind the admin listener rather than the game port, so binding
// that listener to localhost keeps profiling off the public interface. Room
// replay is the tool of first resort for simulation bugs; pprof is for when
// the tick budget is being missed and the question is where the time went.
func registerPprof(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
}
