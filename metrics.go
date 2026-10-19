// gomuks/push - An FCM push gateway for gomuks android.
// Copyright (C) 2024 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"errors"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

const metricsNamespace = "gomuks_push"

// Push tokens are never used as label values: they are bearer credentials for the
// device they belong to, and /metrics is typically unauthenticated. Source IPs are
// never used as label values either, since a public gateway sees an unbounded number
// of them. Both are recorded per-request in the structured access log instead, joined
// to the FCM result lines by request_id.
//
// The owner (Matrix user ID) is bounded by the size of the deployment rather than by
// the internet, so it is available as an opt-in label for gateways with a known,
// small user set. Leave it off for anything public.
var ownerLabelEnabled = os.Getenv("METRICS_OWNER_LABEL") == "true"

// Outcomes of a push that reached FCM.
const (
	resultSent            = "sent"
	resultInvalidToken    = "invalid_token"
	resultQuotaExceeded   = "quota_exceeded"
	resultInvalidArgument = "invalid_argument"
	resultError           = "error"
)

// Reasons a push was rejected before it reached FCM.
const (
	rejectNotFound        = "not_found"
	rejectContentTooLarge = "content_too_large"
	rejectInvalidJSON     = "invalid_json"
	rejectPayloadTooLarge = "payload_too_large"
	rejectInvalidOwner    = "invalid_owner"
	rejectInvalidTarget   = "invalid_target"
	rejectAppIDNotAllowed = "app_id_not_allowed"
)

var registry = prometheus.NewRegistry()

var (
	httpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: "http",
		Name:      "requests_total",
		Help:      "Total number of HTTP requests, by handler, method and response code.",
	}, []string{"handler", "method", "code"})

	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Subsystem: "http",
		Name:      "request_duration_seconds",
		Help:      "End-to-end HTTP request latency, by handler and method.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"handler", "method"})

	httpRequestsInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Subsystem: "http",
		Name:      "requests_in_flight",
		Help:      "Number of HTTP requests currently being served.",
	})

	pushesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "pushes_total",
		Help:      "Total number of pushes forwarded to FCM, by outcome and priority.",
	}, []string{"result", "priority"})

	pushesByOwnerTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "pushes_by_owner_total",
		Help:      "Total number of pushes forwarded to FCM, by owner and outcome. Only registered when METRICS_OWNER_LABEL=true.",
	}, []string{"owner", "result"})

	pushesRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Name:      "pushes_rejected_total",
		Help:      "Total number of pushes rejected before reaching FCM, by reason.",
	}, []string{"reason"})

	fcmSendDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "fcm_send_duration_seconds",
		Help:      "Latency of the upstream FCM send call, by outcome.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"result"})

	pushPayloadBytes = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "push_payload_bytes",
		Help:      "Size of accepted push payloads before base64 encoding.",
		Buckets:   prometheus.ExponentialBuckets(64, 2, 7), // 64B .. 4KiB
	})

	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "build_info",
		Help:      "Build information. Always 1; read the labels.",
	}, []string{"revision", "go_version"})
)

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		httpRequestsTotal,
		httpRequestDuration,
		httpRequestsInFlight,
		pushesTotal,
		pushesRejectedTotal,
		fcmSendDuration,
		pushPayloadBytes,
		buildInfo,
	)
	if ownerLabelEnabled {
		registry.MustRegister(pushesByOwnerTotal)
	}
	buildInfo.WithLabelValues(revision(), goVersion()).Set(1)

	// Pre-create the series that alerting relies on so that a rate() over a
	// freshly started process does not silently return no data.
	for _, priority := range []string{"high", "normal"} {
		for _, result := range []string{resultSent, resultInvalidToken, resultQuotaExceeded, resultInvalidArgument, resultError} {
			pushesTotal.WithLabelValues(result, priority)
		}
	}
	for _, reason := range []string{
		rejectNotFound, rejectContentTooLarge, rejectInvalidJSON,
		rejectPayloadTooLarge, rejectInvalidOwner, rejectInvalidTarget, rejectAppIDNotAllowed,
	} {
		pushesRejectedTotal.WithLabelValues(reason)
	}
}

// version is injected at build time with -ldflags "-X main.version=<sha>". Container
// builds set it explicitly, since the build image has no git binary to stamp
// vcs.revision from.
var version string

func revision() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return "unknown"
}

func goVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.GoVersion
	}
	return "unknown"
}

// recordPush records the outcome of a push that was handed to FCM.
func recordPush(owner, priority, result string, duration time.Duration) {
	pushesTotal.WithLabelValues(result, priority).Inc()
	fcmSendDuration.WithLabelValues(result).Observe(duration.Seconds())
	if ownerLabelEnabled {
		pushesByOwnerTotal.WithLabelValues(owner, result).Inc()
	}
}

// rejectPush records a push rejected before it reached FCM and writes the status code.
func rejectPush(w http.ResponseWriter, reason string, statusCode int) {
	pushesRejectedTotal.WithLabelValues(reason).Inc()
	w.WriteHeader(statusCode)
}

// instrumentHandler wraps h with the HTTP-level metrics, labelled with a stable
// handler name rather than the raw path, which keeps cardinality bounded.
func instrumentHandler(name string, h http.HandlerFunc) http.HandlerFunc {
	labels := prometheus.Labels{"handler": name}
	return promhttp.InstrumentHandlerInFlight(
		httpRequestsInFlight,
		promhttp.InstrumentHandlerDuration(
			httpRequestDuration.MustCurryWith(labels),
			promhttp.InstrumentHandlerCounter(
				httpRequestsTotal.MustCurryWith(labels),
				h,
			),
		),
	).ServeHTTP
}

// metricsHandler renders the registry in the Prometheus text format.
func metricsHandler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
		Registry:      registry,
	})
}

// serveMetrics runs the Prometheus endpoint on its own listener, so that it is not
// exposed alongside the public push endpoint. Set METRICS_LISTEN_ADDRESS to "-" to
// disable it entirely.
func serveMetrics(log *zerolog.Logger) *http.Server {
	addr := os.Getenv("METRICS_LISTEN_ADDRESS")
	if addr == "" {
		addr = ":9090"
	} else if addr == "-" {
		log.Info().Msg("Metrics endpoint disabled")
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metricsHandler())
	server := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Info().Str("listen_address", addr).Msg("Starting metrics server")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Err(err).Msg("Metrics server failed")
		}
	}()
	return server
}
