package service

import (
	"net/http"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metricSource struct {
	runtime *Runtime
	source  string
}

type connectorMetrics struct {
	sources                           []metricSource
	withSource                        bool
	descs                             []*prometheus.Desc
	readyDesc, waitDesc, rejectedDesc *prometheus.Desc
}

var queueWaitBounds = [...]float64{0.01, 0.05, 0.1, 0.5, 1, 5, 15, 60, 300}

// Runtime.mu protects these cumulative observations.
type queueWaitStats struct {
	count   uint64
	sum     float64
	buckets [len(queueWaitBounds)]uint64
}

func (s *queueWaitStats) observe(wait time.Duration) {
	seconds := max(0, wait.Seconds())
	s.count++
	s.sum += seconds
	for i, bound := range queueWaitBounds {
		if seconds <= bound {
			s.buckets[i]++
		}
	}
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
	classes := append(slices.Clone(labels), "class")
	c.readyDesc = prometheus.NewDesc("orpheus_mattermost_ready_threads", "Runnable threads waiting for a worker, excluding backoff and future polls.", classes, nil)
	c.waitDesc = prometheus.NewDesc("orpheus_mattermost_queue_wait_seconds", "Time a runnable thread waited for a worker, excluding backoff.", classes, nil)
	c.rejectedDesc = prometheus.NewDesc("orpheus_mattermost_schedule_rejections_total", "Live notifications not admitted because the thread limit was reached; history replay can rediscover them.", labels, nil)
	return c
}

func (c *connectorMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range c.descs {
		ch <- desc
	}
	ch <- c.readyDesc
	ch <- c.waitDesc
	ch <- c.rejectedDesc
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
		var ready [workClasses]int
		for _, item := range r.runnable(time.Now()) {
			ready[item.class]++
		}
		wait, rejected := r.queueWait, r.queueRejections
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
		ch <- prometheus.MustNewConstMetric(c.rejectedDesc, prometheus.CounterValue, float64(rejected), labels...)
		for class := range workClasses {
			classLabels := append(slices.Clone(labels), class.String())
			ch <- prometheus.MustNewConstMetric(c.readyDesc, prometheus.GaugeValue, float64(ready[class]), classLabels...)
			buckets := make(map[float64]uint64, len(queueWaitBounds))
			for i, bound := range queueWaitBounds {
				buckets[bound] = wait[class].buckets[i]
			}
			ch <- prometheus.MustNewConstHistogram(c.waitDesc, wait[class].count, wait[class].sum, buckets, classLabels...)
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
