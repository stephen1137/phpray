<?php
/**
 * PHPRay Ring Buffer Test
 *
 * Tests ring buffer functionality: writes, stats, capacity.
 * Run with: php -d phpray.output_mode=both -d phpray.trace_cli=1 ringbuffer_test.php
 */

header('Content-Type: application/json');

$results = ['test' => 'ringbuffer', 'passed' => 0, 'failed' => 0, 'details' => []];

function test($name, $condition, &$results) {
    if ($condition) {
        $results['passed']++;
        $results['details'][] = ['name' => $name, 'status' => 'PASS'];
    } else {
        $results['failed']++;
        $results['details'][] = ['name' => $name, 'status' => 'FAIL'];
    }
}

// Test 1: phpray_ring_stats() function exists
test('phpray_ring_stats_exists', function_exists('phpray_ring_stats'), $results);

// Test 2: Get ring buffer stats (should be array if shm mode active, false otherwise)
$stats = phpray_ring_stats();

if ($stats === false) {
    // Ring buffer not active — check if output_mode is "file" (expected)
    $mode = ini_get('phpray.output_mode');
    test('ring_inactive_for_file_mode', $mode === 'file' || $mode === '', $results);
    $results['details'][] = [
        'name' => 'info',
        'status' => 'SKIP',
        'message' => "Ring buffer inactive (output_mode=$mode). Set phpray.output_mode=shm or both to test."
    ];
} else {
    // Ring buffer is active — validate stats structure
    test('stats_is_array', is_array($stats), $results);
    test('stats_has_records', array_key_exists('records', $stats), $results);
    test('stats_has_drops', array_key_exists('drops', $stats), $results);
    test('stats_has_fill_pct', array_key_exists('fill_pct', $stats), $results);
    test('stats_has_capacity', array_key_exists('capacity', $stats), $results);

    // Test 3: Capacity should match configured shm_size minus header
    $configured_size = (int)ini_get('phpray.shm_size');
    test('capacity_positive', $stats['capacity'] > 0, $results);
    test('capacity_less_than_total', $stats['capacity'] < $configured_size, $results);

    // Test 4: Fill percentage should be between 0 and 100
    test('fill_pct_valid', $stats['fill_pct'] >= 0.0 && $stats['fill_pct'] <= 100.0, $results);

    // Test 5: Drops should be a non-negative integer
    test('drops_non_negative', $stats['drops'] >= 0, $results);

    // Test 6: Record count — at least 0 (this request hasn't completed yet so
    // the ring buffer write happens at RSHUTDOWN, not during the request)
    test('records_non_negative', $stats['records'] >= 0, $results);

    // Add stats to output
    $results['ring_stats'] = $stats;
}

// Test: extension is loaded and tracing
test('extension_loaded', extension_loaded('phpray'), $results);
test('is_tracing', phpray_is_tracing(), $results);

// Test: INI values
test('shm_path_set', ini_get('phpray.shm_path') !== false, $results);
test('shm_size_set', (int)ini_get('phpray.shm_size') > 0, $results);
test('output_mode_set', ini_get('phpray.output_mode') !== false, $results);

$results['config'] = [
    'output_mode' => ini_get('phpray.output_mode'),
    'shm_path' => ini_get('phpray.shm_path'),
    'shm_size' => ini_get('phpray.shm_size'),
];

// Generate some work to create trace data
phpray_mark("ringbuffer_test_start");
usleep(50000); // 50ms
phpray_mark("ringbuffer_test_end");

echo json_encode($results, JSON_PRETTY_PRINT) . "\n";
