package service

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metricSource struct {
	runtime *Runtime
	source  string
}

type connectorMetrics struct {
	sources    []metricSource
	withSource bool
	descs      []*prometheus.Desc
}

var connectorMetricNames = []string{
	"pending_inputs", "pending_outputs", "uncertain_admission_age_seconds",
	"reconciliations_total", "errors_total", "reconnects_total", "retries_total",
	"reconcile_duration_seconds_sum", "reconcile_duration_seconds_count",
	"last_success_timestamp_seconds", "replay_lag_seconds", "active_runs",
	"suspended_threads",
}

func newConnectorMetrics(sources []metricSource, withSource bool) *connectorMetrics {
	labels := []string(nil)
	if withSource {
		labels = []string{"source"}
	}
	c := &connectorMetrics{sources: sources, withSource: withSource}
	for _, name := range connectorMetricNames {
		c.descs = append(c.descs, prometheus.NewDesc("orpheus_mattermost_"+name, name, labels, nil))
	}
	return c
}

func (c *connectorMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range c.descs {
		ch <- desc
	}
}

func (c *connectorMetrics) Collect(ch chan<- prometheus.Metric) {
	for _, source := range c.sources {
		r := source.runtime
		inputs, outputs, uncertain := r.Engine.queues()
		r.mu.Lock()
		suspended := 0
		for _, job := range r.jobs {
			if job.Retry.Permanent {
				suspended++
			}
		}
		r.mu.Unlock()
		values := [...]float64{
			float64(inputs), float64(outputs), uncertain.Seconds(),
			float64(r.cycles.Load()), float64(r.failures.Load()), float64(r.reconnects.Load()), float64(r.retries.Load()),
			float64(r.cycleNanos.Load()) / 1e9, float64(r.cycles.Load()),
			float64(r.lastSuccess.Load()), float64(r.replayLag.Load()) / 1e9, float64(r.activeRuns.Load()),
			float64(suspended),
		}
		labels := []string(nil)
		if c.withSource {
			labels = []string{source.source}
		}
		for i, value := range values {
			metricType := prometheus.GaugeValue
			if i >= 3 && i <= 8 {
				metricType = prometheus.CounterValue
			}
			ch <- prometheus.MustNewConstMetric(c.descs[i], metricType, value, labels...)
		}
	}
}

func metricsHandler(sources []metricSource, withSource bool) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newConnectorMetrics(sources, withSource),
	)
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
