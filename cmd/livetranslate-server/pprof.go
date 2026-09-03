package main

import (
	"net"
	"net/http"
	"net/http/pprof"
)

// pprof endpoints: enabled only via PPROF_ENABLED, and then only for
// loopback clients — never exposed through the public listener (the
// reverse proxy is expected to also block /debug/*).

// loopbackOnly rejects non-loopback clients with a plain 404 (revealing
// nothing about the endpoint's existence).
func loopbackOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// pprofGate is a no-op seam kept for symmetry with future auth gating.
func pprofGate(next http.HandlerFunc) http.HandlerFunc { return next }

var (
	pprofIndex   = pprof.Index
	pprofCmdline = pprof.Cmdline
	pprofProfile = pprof.Profile
	pprofSymbol  = pprof.Symbol
	pprofTrace   = pprof.Trace
)
