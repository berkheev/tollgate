package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		defer func() {
			if p := recover(); p != nil {
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
				s.log.Error("panic in handler", "method", r.Method, "path", r.URL.Path, "panic", p)
				if rec.status == 0 {
					writeError(rec, http.StatusInternalServerError, "internal error")
				}
			}

			// The mux fills in r.Pattern; unmatched paths share one label so
			// scanners cannot blow up metric cardinality.
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			elapsed := time.Since(start)
			s.metrics.httpRequests.WithLabelValues(route, strconv.Itoa(rec.status)).Inc()
			s.metrics.httpDuration.WithLabelValues(route).Observe(elapsed.Seconds())
			s.log.Debug("request",
				"method", r.Method,
				"route", route,
				"status", rec.status,
				"duration_ms", float64(elapsed.Microseconds())/1000,
			)
		}()

		next.ServeHTTP(rec, r)
	})
}
