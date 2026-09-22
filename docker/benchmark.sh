#!/bin/bash
# PHPRay Phase 0 Benchmark
# Compares overhead of phpray extension vs baseline

set -e

REQUESTS=1000
CONCURRENCY=10
PORT_PHPRAY=8080
PORT_BASELINE=8081

echo "======================================"
echo " PHPRay Phase 0 — Overhead Benchmark"
echo "======================================"
echo ""
echo "Config: $REQUESTS requests, $CONCURRENCY concurrent"
echo ""

# Cleanup
docker rm -f phpray-test phpray-baseline 2>/dev/null || true

# Start containers
echo "Starting containers..."
docker run -d --name phpray-test -p $PORT_PHPRAY:8080 phpray-test
docker run -d --name phpray-baseline -p $PORT_BASELINE:8080 phpray-baseline
sleep 3

# Verify both are running
echo "Verifying..."
curl -sf http://localhost:$PORT_PHPRAY/health.php > /dev/null || { echo "PHPRay container not ready!"; exit 1; }
curl -sf http://localhost:$PORT_BASELINE/fast.php > /dev/null || { echo "Baseline container not ready!"; exit 1; }
echo "Both containers ready."
echo ""

# Warmup
echo "Warmup (100 requests each)..."
ab -n 100 -c 5 http://localhost:$PORT_PHPRAY/fast.php 2>&1 > /dev/null
ab -n 100 -c 5 http://localhost:$PORT_BASELINE/fast.php 2>&1 > /dev/null
echo ""

# Benchmark fast.php (minimal work — shows pure extension overhead)
echo "======================================"
echo " Test 1: fast.php (minimal work)"
echo "======================================"
echo ""

echo "--- BASELINE (no phpray) ---"
BASELINE_FAST=$(ab -n $REQUESTS -c $CONCURRENCY http://localhost:$PORT_BASELINE/fast.php 2>&1)
BASELINE_FAST_RPS=$(echo "$BASELINE_FAST" | grep "Requests per second" | awk '{print $4}')
BASELINE_FAST_MEAN=$(echo "$BASELINE_FAST" | grep "Time per request.*mean\b" | head -1 | awk '{print $4}')
echo "  RPS: $BASELINE_FAST_RPS"
echo "  Mean time: ${BASELINE_FAST_MEAN}ms"
echo ""

echo "--- PHPRAY (extension loaded) ---"
PHPRAY_FAST=$(ab -n $REQUESTS -c $CONCURRENCY http://localhost:$PORT_PHPRAY/fast.php 2>&1)
PHPRAY_FAST_RPS=$(echo "$PHPRAY_FAST" | grep "Requests per second" | awk '{print $4}')
PHPRAY_FAST_MEAN=$(echo "$PHPRAY_FAST" | grep "Time per request.*mean\b" | head -1 | awk '{print $4}')
echo "  RPS: $PHPRAY_FAST_RPS"
echo "  Mean time: ${PHPRAY_FAST_MEAN}ms"
echo ""

# Calculate overhead
if command -v bc &>/dev/null; then
    OVERHEAD_FAST=$(echo "scale=2; (($PHPRAY_FAST_MEAN - $BASELINE_FAST_MEAN) / $BASELINE_FAST_MEAN) * 100" | bc 2>/dev/null || echo "N/A")
    echo "  Overhead: ${OVERHEAD_FAST}%"
fi
echo ""

# Benchmark index.php (realistic work — 50-500ms)  
echo "======================================"
echo " Test 2: index.php (realistic load)"
echo "======================================"
echo ""

echo "--- BASELINE (no phpray) ---"
BASELINE_REAL=$(ab -n 200 -c 5 http://localhost:$PORT_BASELINE/index.php 2>&1)
BASELINE_REAL_RPS=$(echo "$BASELINE_REAL" | grep "Requests per second" | awk '{print $4}')
BASELINE_REAL_MEAN=$(echo "$BASELINE_REAL" | grep "Time per request.*mean\b" | head -1 | awk '{print $4}')
echo "  RPS: $BASELINE_REAL_RPS"
echo "  Mean time: ${BASELINE_REAL_MEAN}ms"
echo ""

echo "--- PHPRAY (extension loaded) ---"
PHPRAY_REAL=$(ab -n 200 -c 5 http://localhost:$PORT_PHPRAY/index.php 2>&1)
PHPRAY_REAL_RPS=$(echo "$PHPRAY_REAL" | grep "Requests per second" | awk '{print $4}')
PHPRAY_REAL_MEAN=$(echo "$PHPRAY_REAL" | grep "Time per request.*mean\b" | head -1 | awk '{print $4}')
echo "  RPS: $PHPRAY_REAL_RPS"
echo "  Mean time: ${PHPRAY_REAL_MEAN}ms"
echo ""

if command -v bc &>/dev/null; then
    OVERHEAD_REAL=$(echo "scale=2; (($PHPRAY_REAL_MEAN - $BASELINE_REAL_MEAN) / $BASELINE_REAL_MEAN) * 100" | bc 2>/dev/null || echo "N/A")
    echo "  Overhead: ${OVERHEAD_REAL}%"
fi
echo ""

# Check trace output
echo "======================================"
echo " Trace Statistics"
echo "======================================"
TRACE_COUNT=$(docker exec phpray-test wc -l < /tmp/phpray.jsonl)
echo "  Total traces written: $TRACE_COUNT"
echo ""
echo "  By level:"
docker exec phpray-test cat /tmp/phpray.jsonl | jq -r '.level' | sort | uniq -c | sort -rn
echo ""
echo "  File size:"
docker exec phpray-test ls -lh /tmp/phpray.jsonl | awk '{print "  " $5}'
echo ""

# Sample traces
echo "  Sample traces (last 3):"
docker exec phpray-test tail -3 /tmp/phpray.jsonl | jq -c '{host, method, uri, status, duration_ms: (.duration_ms | floor), level}'
echo ""

# Cleanup
echo "Stopping containers..."
docker rm -f phpray-test phpray-baseline 2>/dev/null

echo ""
echo "======================================"
echo " SUMMARY"
echo "======================================"
echo "  Fast requests: Baseline=${BASELINE_FAST_MEAN}ms, PHPRay=${PHPRAY_FAST_MEAN}ms"
echo "  Real requests: Baseline=${BASELINE_REAL_MEAN}ms, PHPRay=${PHPRAY_REAL_MEAN}ms"
if command -v bc &>/dev/null; then
    echo "  Fast overhead: ${OVERHEAD_FAST}%"
    echo "  Real overhead: ${OVERHEAD_REAL}%"
fi
echo "======================================"
