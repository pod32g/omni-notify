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

	// VOICE_CALL channel.
	VoiceCalls             *prometheus.CounterVec
	VoiceCallsAnswered     prometheus.Counter
	VoiceCallsDeclined     prometheus.Counter
	VoiceCallsMissed       prometheus.Counter
	VoiceCallsFailed       prometheus.Counter
	VoiceCallsEnded        *prometheus.CounterVec
	VoiceCallDuration      prometheus.Histogram
	VoiceCallSetup         prometheus.Histogram
	VoiceTimeToAnswer      prometheus.Histogram
	VoiceFirstAudio        prometheus.Histogram
	VoiceSTTLatency        prometheus.Histogram
	VoiceTTSLatency        prometheus.Histogram
	VoiceTurnLatency       prometheus.Histogram
	VoiceTurns             *prometheus.CounterVec
	VoiceScheduled         *prometheus.CounterVec
	VoiceSchedulingFailure *prometheus.CounterVec
	VoicePushes            *prometheus.CounterVec
	VoiceDeferred          *prometheus.CounterVec
	VoiceDuplicates        prometheus.Counter
}

// latencyBuckets suit call setup and speech latencies (50 ms .. 60 s).
var latencyBuckets = []float64{.05, .1, .25, .5, .75, 1, 1.5, 2, 3, 5, 10, 20, 30, 60}

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

		VoiceCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_voice_calls_total",
			Help: "Voice calls created, by origin (api, route, user, retry).",
		}, []string{"origin"}),
		VoiceCallsAnswered: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "omni_notify_voice_calls_answered_total",
			Help: "Voice calls answered on a device.",
		}),
		VoiceCallsDeclined: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "omni_notify_voice_calls_declined_total",
			Help: "Voice calls declined on a device.",
		}),
		VoiceCallsMissed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "omni_notify_voice_calls_missed_total",
			Help: "Voice calls that rang out unanswered.",
		}),
		VoiceCallsFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "omni_notify_voice_calls_failed_total",
			Help: "Voice calls that failed (no device, push failure, audio or app failure, inactivity).",
		}),
		VoiceCallsEnded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_voice_calls_ended_total",
			Help: "Voice calls by final state and outcome.",
		}, []string{"state", "outcome"}),
		VoiceCallDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "omni_notify_voice_call_duration_seconds",
			Help:    "Answered call length, answer to hang-up.",
			Buckets: []float64{5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600},
		}),
		VoiceCallSetup: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "omni_notify_voice_call_setup_duration_seconds",
			Help:    "From a call being due to its invitations being pushed.",
			Buckets: latencyBuckets,
		}),
		VoiceTimeToAnswer: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "omni_notify_voice_time_to_answer_seconds",
			Help:    "From ringing to answered.",
			Buckets: latencyBuckets,
		}),
		VoiceFirstAudio: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "omni_notify_voice_first_audio_latency_seconds",
			Help:    "Device-reported time from answering to the first audio played.",
			Buckets: latencyBuckets,
		}),
		VoiceSTTLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "omni_notify_voice_stt_latency_seconds",
			Help:    "Device-reported speech recognition latency (end of speech to final text).",
			Buckets: latencyBuckets,
		}),
		VoiceTTSLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "omni_notify_voice_tts_latency_seconds",
			Help:    "Device-reported time for Gobbie's voice to render a reply's first sentence.",
			Buckets: latencyBuckets,
		}),
		VoiceTurnLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "omni_notify_voice_turn_latency_seconds",
			Help:    "Server time to interpret a turn, apply its action and phrase the reply.",
			Buckets: latencyBuckets,
		}),
		VoiceTurns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_voice_turns_total",
			Help: "Conversational turns by interpreted intent.",
		}, []string{"intent"}),
		VoiceScheduled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_voice_scheduled_calls_total",
			Help: "Calls scheduled for later, by kind (scheduled, snooze, reschedule, retry).",
		}, []string{"kind"}),
		VoiceSchedulingFailure: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_voice_scheduling_failures_total",
			Help: "Requests to schedule a call that were refused or could not be stored, by reason.",
		}, []string{"reason"}),
		VoicePushes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_voice_push_total",
			Help: "Call invitations by result (sent, failed, bad_token, poll).",
		}, []string{"result"}),
		VoiceDeferred: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omni_notify_voice_deferred_total",
			Help: "Due calls held back by policy, by reason.",
		}, []string{"reason"}),
		VoiceDuplicates: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "omni_notify_voice_duplicates_suppressed_total",
			Help: "Call requests answered with an existing call (idempotency key or same pending message).",
		}),
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
		m.VoiceCalls,
		m.VoiceCallsAnswered,
		m.VoiceCallsDeclined,
		m.VoiceCallsMissed,
		m.VoiceCallsFailed,
		m.VoiceCallsEnded,
		m.VoiceCallDuration,
		m.VoiceCallSetup,
		m.VoiceTimeToAnswer,
		m.VoiceFirstAudio,
		m.VoiceSTTLatency,
		m.VoiceTTSLatency,
		m.VoiceTurnLatency,
		m.VoiceTurns,
		m.VoiceScheduled,
		m.VoiceSchedulingFailure,
		m.VoicePushes,
		m.VoiceDeferred,
		m.VoiceDuplicates,
	)
}
