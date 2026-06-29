// Package metrics defines and registers Omni-Notify's Prometheus collectors.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all collectors. Build one with New and register it on a registry.
type Metrics struct {
	EventsReceived      *prometheus.CounterVec
	EventsDeduplicated  *prometheus.CounterVec
	NotificationsSent   *prometheus.CounterVec
	NotificationsFailed *prometheus.CounterVec
	ProviderErrors      *prometheus.CounterVec
	ActiveStates        prometheus.Gauge
	DeliveryDuration    *prometheus.HistogramVec
}

// New constructs the metric collectors with low-cardinality labels.
func New() *Metrics {
	return &Metrics{
		EventsReceived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_events_received_total",
			Help: "Total events accepted by the ingest API.",
		}, []string{"severity", "status"}),
		EventsDeduplicated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_events_deduplicated_total",
			Help: "Total (event, route) evaluations suppressed by deduplication.",
		}, []string{"route"}),
		NotificationsSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_notifications_sent_total",
			Help: "Total notifications delivered successfully.",
		}, []string{"provider_kind"}),
		NotificationsFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_notifications_failed_total",
			Help: "Total notifications that exhausted all retries (dead).",
		}, []string{"provider_kind"}),
		ProviderErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_provider_errors_total",
			Help: "Total individual provider send errors (across attempts).",
		}, []string{"provider_kind"}),
		ActiveStates: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "omni_notify_active_states",
			Help: "Current number of active (firing) event states.",
		}),
		DeliveryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "omni_notify_delivery_duration_seconds",
			Help:    "Provider send duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"provider_kind"}),
	}
}

// MustRegister registers all collectors on r, panicking on error.
func (m *Metrics) MustRegister(r prometheus.Registerer) {
	r.MustRegister(
		m.EventsReceived,
		m.EventsDeduplicated,
		m.NotificationsSent,
		m.NotificationsFailed,
		m.ProviderErrors,
		m.ActiveStates,
		m.DeliveryDuration,
	)
}
