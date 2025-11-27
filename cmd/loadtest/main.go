/*
I2P WebIRC Load Tester

This tool simulates multiple concurrent users connecting to the WebIRC application
to test its load handling capabilities and identify bottlenecks.

USAGE:
  go run ./cmd/loadtest -url http://localhost:8080 -users 80 -duration 5m

FLAGS:
  -url string
        Target WebIRC URL (default "http://localhost:8080")
  -users int
        Number of concurrent users to simulate (default 80)
  -ramp duration
        Ramp-up duration for spawning users (default 30s)
  -duration duration
        Total test duration (default 5m)
  -msg-interval duration
        Interval between messages per user (default 5s)
  -channel string
        Channel for users to join (default "#loadtest")
  -server string
        Server ID to use (default "postman")
*/
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	targetURL    = flag.String("url", "http://localhost:8080", "Target WebIRC URL")
	numUsers     = flag.Int("users", 80, "Number of concurrent users to simulate")
	rampDuration = flag.Duration("ramp", 30*time.Second, "Ramp-up duration for spawning users")
	testDuration = flag.Duration("duration", 5*time.Minute, "Total test duration after ramp-up")
	msgInterval  = flag.Duration("msg-interval", 5*time.Second, "Message send interval per user")
	channel      = flag.String("channel", "#loadtest", "Channel for users to join")
	serverID     = flag.String("server", "postman", "Server ID to use")
	verbose      = flag.Bool("verbose", false, "Enable verbose logging")
)

// LoadTestMetrics tracks test metrics
type LoadTestMetrics struct {
	// Session metrics
	SessionsCreated atomic.Int64
	SessionsFailed  atomic.Int64

	// Request metrics
	RequestsTotal  atomic.Int64
	RequestsFailed atomic.Int64
	JoinSuccess    atomic.Int64
	JoinFailed     atomic.Int64
	SendSuccess    atomic.Int64
	SendFailed     atomic.Int64

	// Latency tracking
	latencyMu      sync.Mutex
	latencies      []int64 // in milliseconds
	JoinLatencies  []int64
	SendLatencies  []int64
}

// RecordLatency records a request latency
func (m *LoadTestMetrics) RecordLatency(latencyMs int64, reqType string) {
	m.latencyMu.Lock()
	defer m.latencyMu.Unlock()

	m.latencies = append(m.latencies, latencyMs)
	switch reqType {
	case "join":
		m.JoinLatencies = append(m.JoinLatencies, latencyMs)
	case "send":
		m.SendLatencies = append(m.SendLatencies, latencyMs)
	}
}

// LatencyStats returns latency statistics
func (m *LoadTestMetrics) LatencyStats() (avg, p50, p95, p99, max float64) {
	m.latencyMu.Lock()
	defer m.latencyMu.Unlock()

	if len(m.latencies) == 0 {
		return 0, 0, 0, 0, 0
	}

	sorted := make([]int64, len(m.latencies))
	copy(sorted, m.latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum int64
	for _, v := range sorted {
		sum += v
	}
	avg = float64(sum) / float64(len(sorted))

	p50 = float64(sorted[len(sorted)*50/100])
	p95 = float64(sorted[len(sorted)*95/100])
	p99 = float64(sorted[len(sorted)*99/100])
	max = float64(sorted[len(sorted)-1])

	return
}

func main() {
	flag.Parse()

	log.Printf("=== I2P WebIRC Load Test ===")
	log.Printf("Target: %s", *targetURL)
	log.Printf("Users: %d", *numUsers)
	log.Printf("Ramp-up: %v", *rampDuration)
	log.Printf("Duration: %v", *testDuration)
	log.Printf("Message interval: %v", *msgInterval)
	log.Printf("Channel: %s", *channel)
	log.Printf("Server: %s", *serverID)
	log.Println()

	metrics := &LoadTestMetrics{}
	var wg sync.WaitGroup

	// Calculate user spawn interval
	spawnInterval := *rampDuration / time.Duration(*numUsers)
	if spawnInterval < time.Millisecond {
		spawnInterval = time.Millisecond
	}

	log.Printf("Spawning %d users over %v (one every %v)", *numUsers, *rampDuration, spawnInterval)

	stopCh := make(chan struct{})
	startTime := time.Now()

	// Progress reporter
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				printProgress(metrics, time.Since(startTime))
			}
		}
	}()

	// Spawn users gradually
	for i := 0; i < *numUsers; i++ {
		wg.Add(1)
		go simulateUser(i, metrics, stopCh, &wg)
		time.Sleep(spawnInterval)
	}

	log.Printf("All %d users spawned, running test for %v...", *numUsers, *testDuration)

	// Wait for test duration
	time.Sleep(*testDuration)

	log.Println("Stopping users...")
	close(stopCh)
	wg.Wait()

	// Print final results
	printResults(metrics, time.Since(startTime))
}

func simulateUser(id int, metrics *LoadTestMetrics, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	nick := fmt.Sprintf("loadtest%d", id)

	// Create HTTP client with cookie jar
	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Printf("User %d: failed to create cookie jar: %v", id, err)
		metrics.SessionsFailed.Add(1)
		return
	}
	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Allow redirects but track them
			return nil
		},
	}

	// Get CSRF token by visiting the index page
	csrfToken, err := getCSRFToken(client, *targetURL)
	if err != nil {
		if *verbose {
			log.Printf("User %d: failed to get CSRF token: %v", id, err)
		}
		metrics.SessionsFailed.Add(1)
		return
	}

	// Join channel
	start := time.Now()
	if err := joinChannel(client, *targetURL, nick, *channel, *serverID, csrfToken); err != nil {
		if *verbose {
			log.Printf("User %d: failed to join: %v", id, err)
		}
		metrics.SessionsFailed.Add(1)
		metrics.JoinFailed.Add(1)
		return
	}
	metrics.JoinSuccess.Add(1)
	metrics.RecordLatency(time.Since(start).Milliseconds(), "join")
	metrics.SessionsCreated.Add(1)

	if *verbose {
		log.Printf("User %d joined successfully", id)
	}

	// Send messages periodically
	ticker := time.NewTicker(*msgInterval)
	defer ticker.Stop()

	msgNum := 0
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			msgNum++
			msg := fmt.Sprintf("Test message %d from user %d at %s", msgNum, id, time.Now().Format("15:04:05"))

			start := time.Now()
			metrics.RequestsTotal.Add(1)

			if err := sendMessage(client, *targetURL, *channel, msg, csrfToken); err != nil {
				if *verbose {
					log.Printf("User %d: send failed: %v", id, err)
				}
				metrics.SendFailed.Add(1)
				metrics.RequestsFailed.Add(1)
			} else {
				metrics.SendSuccess.Add(1)
				metrics.RecordLatency(time.Since(start).Milliseconds(), "send")
			}
		}
	}
}

func getCSRFToken(client *http.Client, baseURL string) (string, error) {
	resp, err := client.Get(baseURL + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Consume the body
	io.Copy(io.Discard, resp.Body)

	// Extract CSRF token from cookies (the server sets it as a cookie)
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}

	cookies := client.Jar.Cookies(parsedURL)
	for _, cookie := range cookies {
		if cookie.Name == "csrf_token" {
			return cookie.Value, nil
		}
	}

	return "", fmt.Errorf("CSRF token cookie not found")
}

func joinChannel(client *http.Client, baseURL, nick, channel, server, csrfToken string) error {
	form := url.Values{}
	form.Set("nick", nick)
	form.Set("channel", channel)
	form.Set("server", server)
	form.Set("csrf_token", csrfToken)

	resp, err := client.PostForm(baseURL+"/join", form)
	if err != nil {
		return fmt.Errorf("POST /join failed: %w", err)
	}
	defer resp.Body.Close()

	// Consume body
	io.Copy(io.Discard, resp.Body)

	// Accept 200, 302, or 303 as success (redirects to channel page)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("join returned status %d", resp.StatusCode)
	}

	return nil
}

func sendMessage(client *http.Client, baseURL, channel, message, csrfToken string) error {
	form := url.Values{}
	form.Set("channel", channel)
	form.Set("message", message)
	form.Set("csrf_token", csrfToken)

	resp, err := client.PostForm(baseURL+"/send", form)
	if err != nil {
		return fmt.Errorf("POST /send failed: %w", err)
	}
	defer resp.Body.Close()

	// Consume body
	io.Copy(io.Discard, resp.Body)

	// Accept 200, 302, or 303 as success
	if resp.StatusCode >= 400 {
		return fmt.Errorf("send returned status %d", resp.StatusCode)
	}

	return nil
}

func printProgress(metrics *LoadTestMetrics, elapsed time.Duration) {
	sessions := metrics.SessionsCreated.Load()
	failed := metrics.SessionsFailed.Load()
	requests := metrics.RequestsTotal.Load()
	reqFailed := metrics.RequestsFailed.Load()

	log.Printf("[%v] Sessions: %d created, %d failed | Requests: %d total, %d failed",
		elapsed.Round(time.Second), sessions, failed, requests, reqFailed)
}

func printResults(metrics *LoadTestMetrics, elapsed time.Duration) {
	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("           LOAD TEST RESULTS           ")
	fmt.Println("========================================")
	fmt.Println()

	fmt.Printf("Duration: %v\n", elapsed.Round(time.Second))
	fmt.Printf("Target: %s\n", *targetURL)
	fmt.Println()

	fmt.Println("--- Session Metrics ---")
	fmt.Printf("  Created:  %d\n", metrics.SessionsCreated.Load())
	fmt.Printf("  Failed:   %d\n", metrics.SessionsFailed.Load())
	fmt.Println()

	fmt.Println("--- Request Metrics ---")
	fmt.Printf("  Total:    %d\n", metrics.RequestsTotal.Load())
	fmt.Printf("  Failed:   %d\n", metrics.RequestsFailed.Load())
	fmt.Printf("  Join OK:  %d\n", metrics.JoinSuccess.Load())
	fmt.Printf("  Join Fail:%d\n", metrics.JoinFailed.Load())
	fmt.Printf("  Send OK:  %d\n", metrics.SendSuccess.Load())
	fmt.Printf("  Send Fail:%d\n", metrics.SendFailed.Load())
	fmt.Println()

	// Calculate error rate
	total := metrics.RequestsTotal.Load()
	failed := metrics.RequestsFailed.Load()
	if total > 0 {
		errorRate := float64(failed) / float64(total) * 100
		fmt.Printf("Error Rate: %.2f%%\n", errorRate)
	}
	fmt.Println()

	// Latency stats
	avg, p50, p95, p99, max := metrics.LatencyStats()
	fmt.Println("--- Latency (ms) ---")
	fmt.Printf("  Average:  %.2f\n", avg)
	fmt.Printf("  p50:      %.2f\n", p50)
	fmt.Printf("  p95:      %.2f\n", p95)
	fmt.Printf("  p99:      %.2f\n", p99)
	fmt.Printf("  Max:      %.2f\n", max)
	fmt.Println()

	// Try to fetch server metrics
	fetchServerMetrics(*targetURL)
}

func fetchServerMetrics(baseURL string) {
	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		log.Printf("Could not fetch server metrics: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var serverMetrics map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&serverMetrics); err != nil {
		return
	}

	fmt.Println("--- Server Metrics ---")
	if v, ok := serverMetrics["sessions_active"]; ok {
		fmt.Printf("  Active Sessions: %.0f\n", v)
	}
	if v, ok := serverMetrics["goroutines"]; ok {
		fmt.Printf("  Goroutines:      %.0f\n", v)
	}
	if v, ok := serverMetrics["heap_alloc_mb"]; ok {
		fmt.Printf("  Heap (MB):       %.0f\n", v)
	}
	fmt.Println()
}
