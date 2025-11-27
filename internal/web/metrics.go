package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/dustinfields/i2p-irc/internal/irc"
)

// Metrics collects and exposes application metrics for monitoring and load testing
type Metrics struct {
	// Counters (monotonically increasing)
	RequestsTotal    atomic.Int64
	SessionsCreated  atomic.Int64
	SessionsClosed   atomic.Int64
	MessagesSent     atomic.Int64
	IRCReconnects    atomic.Int64
	RateLimitHits    atomic.Int64
	JoinAttempts     atomic.Int64
	JoinFailures     atomic.Int64
	CapacityRejected atomic.Int64

	// Gauges (set periodically by background goroutine)
	ActiveSessions atomic.Int64
	GoroutineCount atomic.Int64
	HeapAllocMB    atomic.Int64
	HeapSysMB      atomic.Int64

	// Latency tracking (simple average)
	LatencySumMs atomic.Int64
	LatencyCount atomic.Int64
	LatencyMaxMs atomic.Int64

	StartTime time.Time
	stopCh    chan struct{}
}

// NewMetrics creates a new metrics collector
func NewMetrics() *Metrics {
	m := &Metrics{
		StartTime: time.Now(),
		stopCh:    make(chan struct{}),
	}
	go m.updateGauges()
	return m
}

// Stop stops the background gauge updater
func (m *Metrics) Stop() {
	close(m.stopCh)
}

// updateGauges periodically updates gauge metrics
func (m *Metrics) updateGauges() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			m.GoroutineCount.Store(int64(runtime.NumGoroutine()))
			m.HeapAllocMB.Store(int64(mem.HeapAlloc / 1024 / 1024))
			m.HeapSysMB.Store(int64(mem.HeapSys / 1024 / 1024))
		}
	}
}

// RecordLatency records a request latency in milliseconds
func (m *Metrics) RecordLatency(ms int64) {
	m.LatencySumMs.Add(ms)
	m.LatencyCount.Add(1)

	// Update max (non-atomic but close enough for monitoring)
	for {
		current := m.LatencyMaxMs.Load()
		if ms <= current {
			break
		}
		if m.LatencyMaxMs.CompareAndSwap(current, ms) {
			break
		}
	}
}

// GetAverageLatencyMs returns the average latency in milliseconds
func (m *Metrics) GetAverageLatencyMs() float64 {
	count := m.LatencyCount.Load()
	if count == 0 {
		return 0
	}
	return float64(m.LatencySumMs.Load()) / float64(count)
}

// MetricsResponse represents the JSON response for /metrics endpoint
type MetricsResponse struct {
	UptimeSeconds    float64        `json:"uptime_seconds"`
	RequestsTotal    int64          `json:"requests_total"`
	SessionsCreated  int64          `json:"sessions_created"`
	SessionsClosed   int64          `json:"sessions_closed"`
	SessionsActive   int64          `json:"sessions_active"`
	MessagesSent     int64          `json:"messages_sent"`
	IRCReconnects    int64          `json:"irc_reconnects"`
	RateLimitHits    int64          `json:"rate_limit_hits"`
	JoinAttempts     int64          `json:"join_attempts"`
	JoinFailures     int64          `json:"join_failures"`
	CapacityRejected int64          `json:"capacity_rejected"`
	Goroutines       int64          `json:"goroutines"`
	HeapAllocMB      int64          `json:"heap_alloc_mb"`
	HeapSysMB        int64          `json:"heap_sys_mb"`
	Latency          LatencyMetrics `json:"latency"`
}

// LatencyMetrics represents latency statistics
type LatencyMetrics struct {
	AverageMs float64 `json:"average_ms"`
	MaxMs     int64   `json:"max_ms"`
	Count     int64   `json:"count"`
}

// Handler returns an HTTP handler for the /metrics endpoint
func (m *Metrics) Handler(sessions *irc.SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Update active sessions gauge
		m.ActiveSessions.Store(int64(sessions.Count()))

		resp := MetricsResponse{
			UptimeSeconds:    time.Since(m.StartTime).Seconds(),
			RequestsTotal:    m.RequestsTotal.Load(),
			SessionsCreated:  m.SessionsCreated.Load(),
			SessionsClosed:   m.SessionsClosed.Load(),
			SessionsActive:   m.ActiveSessions.Load(),
			MessagesSent:     m.MessagesSent.Load(),
			IRCReconnects:    m.IRCReconnects.Load(),
			RateLimitHits:    m.RateLimitHits.Load(),
			JoinAttempts:     m.JoinAttempts.Load(),
			JoinFailures:     m.JoinFailures.Load(),
			CapacityRejected: m.CapacityRejected.Load(),
			Goroutines:       m.GoroutineCount.Load(),
			HeapAllocMB:      m.HeapAllocMB.Load(),
			HeapSysMB:        m.HeapSysMB.Load(),
			Latency: LatencyMetrics{
				AverageMs: m.GetAverageLatencyMs(),
				MaxMs:     m.LatencyMaxMs.Load(),
				Count:     m.LatencyCount.Load(),
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// PrometheusHandler returns an HTTP handler for Prometheus-style metrics
func (m *Metrics) PrometheusHandler(sessions *irc.SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Update active sessions gauge
		m.ActiveSessions.Store(int64(sessions.Count()))

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")

		output := fmt.Sprintf(`# HELP webirc_requests_total Total HTTP requests
# TYPE webirc_requests_total counter
webirc_requests_total %d

# HELP webirc_sessions_created_total Total sessions created
# TYPE webirc_sessions_created_total counter
webirc_sessions_created_total %d

# HELP webirc_sessions_closed_total Total sessions closed
# TYPE webirc_sessions_closed_total counter
webirc_sessions_closed_total %d

# HELP webirc_messages_sent_total Total messages sent
# TYPE webirc_messages_sent_total counter
webirc_messages_sent_total %d

# HELP webirc_irc_reconnects_total Total IRC reconnection attempts
# TYPE webirc_irc_reconnects_total counter
webirc_irc_reconnects_total %d

# HELP webirc_rate_limit_hits_total Total rate limit hits
# TYPE webirc_rate_limit_hits_total counter
webirc_rate_limit_hits_total %d

# HELP webirc_join_attempts_total Total join attempts
# TYPE webirc_join_attempts_total counter
webirc_join_attempts_total %d

# HELP webirc_join_failures_total Total join failures
# TYPE webirc_join_failures_total counter
webirc_join_failures_total %d

# HELP webirc_capacity_rejected_total Total capacity rejections
# TYPE webirc_capacity_rejected_total counter
webirc_capacity_rejected_total %d

# HELP webirc_sessions_active Current active sessions
# TYPE webirc_sessions_active gauge
webirc_sessions_active %d

# HELP webirc_goroutines Current goroutine count
# TYPE webirc_goroutines gauge
webirc_goroutines %d

# HELP webirc_heap_alloc_mb Current heap allocation in MB
# TYPE webirc_heap_alloc_mb gauge
webirc_heap_alloc_mb %d

# HELP webirc_uptime_seconds Server uptime in seconds
# TYPE webirc_uptime_seconds gauge
webirc_uptime_seconds %.2f

# HELP webirc_latency_avg_ms Average request latency in milliseconds
# TYPE webirc_latency_avg_ms gauge
webirc_latency_avg_ms %.2f

# HELP webirc_latency_max_ms Maximum request latency in milliseconds
# TYPE webirc_latency_max_ms gauge
webirc_latency_max_ms %d
`,
			m.RequestsTotal.Load(),
			m.SessionsCreated.Load(),
			m.SessionsClosed.Load(),
			m.MessagesSent.Load(),
			m.IRCReconnects.Load(),
			m.RateLimitHits.Load(),
			m.JoinAttempts.Load(),
			m.JoinFailures.Load(),
			m.CapacityRejected.Load(),
			m.ActiveSessions.Load(),
			m.GoroutineCount.Load(),
			m.HeapAllocMB.Load(),
			time.Since(m.StartTime).Seconds(),
			m.GetAverageLatencyMs(),
			m.LatencyMaxMs.Load(),
		)
		w.Write([]byte(output))
	}
}
