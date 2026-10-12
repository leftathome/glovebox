package loginbroker

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics are the broker's Prometheus series, on a registry of their own.
// Every label value comes from a closed list in this package or in
// brokerapi; nothing a caller or a site sends becomes a label.
//
// All methods are safe on a nil *Metrics.
type Metrics struct {
	registry *prometheus.Registry

	requests     *prometheus.CounterVec
	joins        prometheus.Counter
	authFailures *prometheus.CounterVec
	logins       *prometheus.CounterVec
	submissions  prometheus.Counter
	probes       *prometheus.CounterVec
	latched      prometheus.Gauge
	lastLoginAt  prometheus.Gauge
	today        prometheus.Gauge
}

// NewMetrics builds the series for one site.
func NewMetrics(site string) *Metrics {
	labels := prometheus.Labels{"site": site}
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "login_broker_refresh_total",
			Help:        "Refreshes performed, by outcome and code. Requests that joined a refresh in flight are not counted here.",
			ConstLabels: labels,
		}, []string{"outcome", "code"}),
		joins: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "login_broker_refresh_joined_total",
			Help:        "Requests answered by a refresh another request had already started.",
			ConstLabels: labels,
		}),
		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "login_broker_request_refused_total",
			Help:        "Requests refused before any policy ran, by reason.",
			ConstLabels: labels,
		}, []string{"reason"}),
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "login_broker_login_total",
			Help:        "Logins by result: success, rejected, login_failed, login_unconfirmed, not_submitted.",
			ConstLabels: labels,
		}, []string{"result"}),
		submissions: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "login_broker_login_attempt_total",
			Help:        "Calls of the site driver's login. Each submits the credentials at most once.",
			ConstLabels: labels,
		}),
		probes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "login_broker_probe_total",
			Help:        "Read-only checks of a session against the site, by result.",
			ConstLabels: labels,
		}, []string{"result"}),
		latched: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "login_broker_latched",
			Help:        "1 when the last refresh found or set the login latch; 0 when it found it clear. Not set before the first request.",
			ConstLabels: labels,
		}),
		lastLoginAt: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "login_broker_last_login_timestamp_seconds",
			Help:        "When this process last logged in successfully (unix time). 0 until it has.",
			ConstLabels: labels,
		}),
	}
	m.today = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "login_broker_logins_today",
		Help:        "Logins counted against the daily budget for the current local day, by every workload sharing the account's record, as of the last refresh. A refresh answered budget_exhausted is counted in login_broker_refresh_total{outcome=\"budget_exhausted\"}.",
		ConstLabels: labels,
	})
	m.registry.MustRegister(m.requests, m.joins, m.authFailures, m.logins, m.submissions, m.probes, m.latched, m.lastLoginAt, m.today)
	return m
}

// Handler serves the registry.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) request(outcome, code string) {
	if m != nil {
		m.requests.WithLabelValues(outcome, code).Inc()
	}
}

func (m *Metrics) joined() {
	if m != nil {
		m.joins.Inc()
	}
}

func (m *Metrics) refused(reason string) {
	if m != nil {
		m.authFailures.WithLabelValues(reason).Inc()
	}
}

func (m *Metrics) loginResult(result string) {
	if m != nil {
		m.logins.WithLabelValues(result).Inc()
	}
}

func (m *Metrics) loginAttempt() {
	if m != nil {
		m.submissions.Inc()
	}
}

func (m *Metrics) probe(state ProbeState) {
	if m != nil {
		m.probes.WithLabelValues(state.String()).Inc()
	}
}

func (m *Metrics) setLatched(on bool) {
	if m == nil {
		return
	}
	if on {
		m.latched.Set(1)
		return
	}
	m.latched.Set(0)
}

func (m *Metrics) lastLogin(at time.Time) {
	if m != nil {
		m.lastLoginAt.Set(float64(at.Unix()))
	}
}

func (m *Metrics) loginsToday(n int) {
	if m != nil {
		m.today.Set(float64(n))
	}
}
