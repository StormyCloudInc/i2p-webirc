#!/bin/bash
# Real I2P WebIRC Load Test Script
#
# This script tests actual I2P connectivity by spawning multiple curl-based
# users that connect, join channels, and send messages through the WebIRC interface.
#
# USAGE:
#   ./load_test_real.sh [URL] [NUM_USERS] [RAMP_SECONDS] [TEST_DURATION_SECONDS]
#
# EXAMPLES:
#   ./load_test_real.sh http://localhost:8080 10 60 300
#   ./load_test_real.sh http://irc.i2p.net 20 120 600
#
# NOTE: For real I2P testing, start with fewer users (5-10) and shorter
# durations since I2P tunnel creation adds latency.

set -e

# Configuration
TARGET_URL="${1:-http://localhost:8080}"
NUM_USERS="${2:-10}"
RAMP_SECONDS="${3:-60}"
TEST_DURATION="${4:-300}"
MSG_INTERVAL="${5:-5}"
CHANNEL="${6:-#loadtest}"
SERVER="${7:-postman}"

# Calculate spawn interval
if command -v bc &> /dev/null; then
    SPAWN_INTERVAL=$(echo "scale=2; $RAMP_SECONDS / $NUM_USERS" | bc)
else
    SPAWN_INTERVAL=$((RAMP_SECONDS / NUM_USERS))
fi

# Create results directory
RESULTS_DIR="/tmp/loadtest_$(date +%s)"
mkdir -p "$RESULTS_DIR"

echo "=========================================="
echo "   I2P WebIRC Real Load Test"
echo "=========================================="
echo ""
echo "Target URL:      $TARGET_URL"
echo "Users:           $NUM_USERS"
echo "Ramp-up:         ${RAMP_SECONDS}s (one every ${SPAWN_INTERVAL}s)"
echo "Test duration:   ${TEST_DURATION}s"
echo "Message interval: ${MSG_INTERVAL}s"
echo "Channel:         $CHANNEL"
echo "Server:          $SERVER"
echo "Results:         $RESULTS_DIR"
echo ""

# Get CSRF token from cookies (the server sets it as a cookie, not in HTML)
get_csrf_token() {
    local cookie_file="$1"
    # First request to get the cookies set
    curl -s -c "$cookie_file" "$TARGET_URL/" > /dev/null
    # Extract csrf_token from the cookie file (Netscape format: domain, flag, path, secure, expiry, name, value)
    grep -E 'csrf_token' "$cookie_file" | awk '{print $NF}' | head -1
}

# Simulate a single user
simulate_user() {
    local user_id=$1
    local nick="loadtest${user_id}"
    local cookie_file="$RESULTS_DIR/cookies_${user_id}"
    local log_file="$RESULTS_DIR/user_${user_id}.log"
    local metrics_file="$RESULTS_DIR/user_${user_id}_metrics.txt"

    echo "0" > "$metrics_file.success"
    echo "0" > "$metrics_file.failed"
    echo "0" > "$metrics_file.latency_sum"

    # Get CSRF token
    local csrf_token
    csrf_token=$(get_csrf_token "$cookie_file")

    if [ -z "$csrf_token" ]; then
        echo "User $user_id: Failed to get CSRF token" >> "$log_file"
        echo "1" > "$metrics_file.join_failed"
        return 1
    fi

    echo "User $user_id: Got CSRF token" >> "$log_file"

    # URL encode channel (# -> %23)
    local encoded_channel
    encoded_channel=$(echo "$CHANNEL" | sed 's/#/%23/g')

    # Join channel
    local join_start join_end join_status
    join_start=$(date +%s%3N)
    join_status=$(curl -s -c "$cookie_file" -b "$cookie_file" \
        -X POST "$TARGET_URL/join" \
        -d "nick=$nick&channel=$encoded_channel&server=$SERVER&csrf_token=$csrf_token" \
        -o /dev/null -w "%{http_code}" 2>&1)
    join_end=$(date +%s%3N)

    local join_latency=$((join_end - join_start))

    if [[ "$join_status" -ge 400 ]]; then
        echo "User $user_id: Join failed with status $join_status (${join_latency}ms)" >> "$log_file"
        echo "1" > "$metrics_file.join_failed"
        return 1
    fi

    echo "User $user_id: Joined successfully (${join_latency}ms)" >> "$log_file"
    echo "0" > "$metrics_file.join_failed"

    # Send messages until test ends
    local msg_count=0
    local end_time=$(($(date +%s) + TEST_DURATION))

    while [ "$(date +%s)" -lt "$end_time" ]; do
        ((msg_count++))

        local msg_start msg_end msg_status
        msg_start=$(date +%s%3N)
        msg_status=$(curl -s -b "$cookie_file" -c "$cookie_file" \
            -X POST "$TARGET_URL/send" \
            -d "message=Test+message+$msg_count+from+$nick&channel=$encoded_channel&csrf_token=$csrf_token" \
            -o /dev/null -w "%{http_code}" 2>&1)
        msg_end=$(date +%s%3N)

        local msg_latency=$((msg_end - msg_start))

        if [[ "$msg_status" -lt 400 ]]; then
            echo "User $user_id: Sent message $msg_count (${msg_latency}ms)" >> "$log_file"
            # Update success counter
            local current_success current_latency
            current_success=$(cat "$metrics_file.success")
            echo $((current_success + 1)) > "$metrics_file.success"
            current_latency=$(cat "$metrics_file.latency_sum")
            echo $((current_latency + msg_latency)) > "$metrics_file.latency_sum"
        else
            echo "User $user_id: Message $msg_count failed with $msg_status" >> "$log_file"
            local current_failed
            current_failed=$(cat "$metrics_file.failed")
            echo $((current_failed + 1)) > "$metrics_file.failed"
        fi

        sleep "$MSG_INTERVAL"
    done

    echo "User $user_id: Finished after $msg_count messages" >> "$log_file"
}

# Track spawned user PIDs
declare -a PIDS

# Progress reporter
report_progress() {
    while true; do
        sleep 10
        local active_users
        active_users=$(jobs -r | wc -l)
        local elapsed=$(($(date +%s) - START_TIME))
        echo "[${elapsed}s] Active users: $active_users"
    done
}

START_TIME=$(date +%s)

# Start progress reporter in background
report_progress &
PROGRESS_PID=$!

echo "Spawning $NUM_USERS users..."

# Spawn users gradually
for i in $(seq 1 "$NUM_USERS"); do
    simulate_user "$i" &
    PIDS+=($!)
    echo "Spawned user $i (PID: ${PIDS[-1]})"
    sleep "$SPAWN_INTERVAL"
done

echo ""
echo "All $NUM_USERS users spawned, running for ${TEST_DURATION}s..."
echo ""

# Wait for all users to complete
for pid in "${PIDS[@]}"; do
    wait "$pid" 2>/dev/null || true
done

# Stop progress reporter
kill "$PROGRESS_PID" 2>/dev/null || true

END_TIME=$(date +%s)
ELAPSED=$((END_TIME - START_TIME))

echo ""
echo "=========================================="
echo "           LOAD TEST RESULTS"
echo "=========================================="
echo ""
echo "Duration: ${ELAPSED}s"
echo "Target:   $TARGET_URL"
echo ""

# Aggregate results
total_success=0
total_failed=0
total_latency=0
join_success=0
join_failed=0

for i in $(seq 1 "$NUM_USERS"); do
    metrics_file="$RESULTS_DIR/user_${i}_metrics.txt"

    if [ -f "$metrics_file.success" ]; then
        success=$(cat "$metrics_file.success")
        total_success=$((total_success + success))
    fi

    if [ -f "$metrics_file.failed" ]; then
        failed=$(cat "$metrics_file.failed")
        total_failed=$((total_failed + failed))
    fi

    if [ -f "$metrics_file.latency_sum" ]; then
        latency=$(cat "$metrics_file.latency_sum")
        total_latency=$((total_latency + latency))
    fi

    if [ -f "$metrics_file.join_failed" ]; then
        jf=$(cat "$metrics_file.join_failed")
        if [ "$jf" -eq 0 ]; then
            ((join_success++))
        else
            ((join_failed++))
        fi
    fi
done

echo "--- Session Metrics ---"
echo "  Join Success: $join_success"
echo "  Join Failed:  $join_failed"
echo ""

echo "--- Message Metrics ---"
echo "  Sent OK:      $total_success"
echo "  Send Failed:  $total_failed"
echo ""

# Calculate error rate
total_requests=$((total_success + total_failed))
if [ "$total_requests" -gt 0 ]; then
    error_rate=$(echo "scale=2; $total_failed * 100 / $total_requests" | bc 2>/dev/null || echo "N/A")
    avg_latency=$(echo "scale=2; $total_latency / $total_success" | bc 2>/dev/null || echo "N/A")
    echo "Error Rate:     ${error_rate}%"
    echo "Avg Latency:    ${avg_latency}ms"
fi
echo ""

# Try to fetch server metrics
echo "--- Server Metrics ---"
server_metrics=$(curl -s "$TARGET_URL/metrics" 2>/dev/null || echo "{}")
if [ -n "$server_metrics" ] && [ "$server_metrics" != "{}" ]; then
    echo "$server_metrics" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(f'  Active Sessions: {d.get(\"sessions_active\", \"N/A\")}')
    print(f'  Goroutines:      {d.get(\"goroutines\", \"N/A\")}')
    print(f'  Heap (MB):       {d.get(\"heap_alloc_mb\", \"N/A\")}')
    print(f'  Total Requests:  {d.get(\"requests_total\", \"N/A\")}')
except:
    print('  Could not parse server metrics')
" 2>/dev/null || echo "  Could not fetch server metrics"
else
    echo "  Could not fetch server metrics"
fi
echo ""

echo "Detailed logs: $RESULTS_DIR"
echo ""

# Show sample of user logs
echo "--- Sample User Logs ---"
for i in 1 2 3; do
    if [ -f "$RESULTS_DIR/user_${i}.log" ]; then
        echo "User $i:"
        tail -3 "$RESULTS_DIR/user_${i}.log" | sed 's/^/  /'
    fi
done
echo ""
