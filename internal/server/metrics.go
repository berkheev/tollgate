package server

import (
	"github.com/prometheus/client_golang/prometheus"
)

type metrics struct {
	decisions     *prometheus.CounterVec
	checkDuration *prometheus.HistogramVec
	backendErrors *prometheus.CounterVec
	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
	rules         prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tollgate_decisions_total",
			Help: "Rate limit decisions by rule and outcome.",
		}, []string{"rule", "decision"}),
		checkDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tollgate_check_duration_seconds",
			Help:    "Time spent in the backend per check.",
			Buckets: []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25},
		}, []string{"rule"}),
		backendErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tollgate_backend_errors_total",
			Help: "Backend failures by operation.",
		}, []string{"op"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tollgate_http_requests_total",
			Help: "HTTP requests by route and status code.",
		}, []string{"route", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tollgate_http_request_duration_seconds",
			Help:    "HTTP request latency by route.",
			Buckets: []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
		}, []string{"route"}),
		rules: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tollgate_rules",
			Help: "Number of loaded rules.",
		}),
	}
	reg.MustRegister(m.decisions, m.checkDuration, m.backendErrors, m.httpRequests, m.httpDuration, m.rules)
	return m
}
