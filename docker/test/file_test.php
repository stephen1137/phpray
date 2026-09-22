<?php
/**
 * PHPRay File I/O Hook Test
 * 
 * Tests:
 * 1. file_get_contents on local files
 * 2. file_put_contents (write)
 * 3. file_get_contents on URLs (should be recorded as HTTP calls)
 * 4. file_get_contents on non-existent file (error case)
 */

header('Content-Type: application/json');

$results = [];

// Mark: start file tests
if (function_exists('phpray_mark')) {
    phpray_mark('file_tests_start');
}

// 1. Read a local file
$start = microtime(true);
$content = file_get_contents('/etc/hostname');
$results['read_hostname'] = [
    'success' => $content !== false,
    'bytes' => $content !== false ? strlen($content) : 0,
    'time_ms' => round((microtime(true) - $start) * 1000, 3)
];

// 2. Read PHP's own source (larger file)
$start = microtime(true);
$content = file_get_contents(__FILE__);
$results['read_self'] = [
    'success' => $content !== false,
    'bytes' => strlen($content),
    'time_ms' => round((microtime(true) - $start) * 1000, 3)
];

// 3. Write a temp file
$tmp = tempnam('/tmp', 'phpray_test_');
$data = str_repeat('PHPRay file write test. ', 100); // ~2.4KB
$start = microtime(true);
$bytes = file_put_contents($tmp, $data);
$results['write_temp'] = [
    'success' => $bytes !== false,
    'bytes' => $bytes,
    'time_ms' => round((microtime(true) - $start) * 1000, 3)
];

// 4. Read the file we just wrote
$start = microtime(true);
$readback = file_get_contents($tmp);
$results['read_temp'] = [
    'success' => $readback !== false,
    'bytes' => strlen($readback),
    'matches' => ($readback === $data),
    'time_ms' => round((microtime(true) - $start) * 1000, 3)
];

// Cleanup temp file
unlink($tmp);

// 5. file_get_contents on a URL — should appear as HTTP call, not file op
// Note: can't call localhost:8080 from PHP built-in server (single-threaded deadlock)
// Use an external URL or a known-reachable endpoint
$start = microtime(true);
$url_content = @file_get_contents('http://httpbin.org/get');
$results['http_via_fgc'] = [
    'success' => $url_content !== false,
    'bytes' => $url_content !== false ? strlen($url_content) : 0,
    'time_ms' => round((microtime(true) - $start) * 1000, 3),
    'note' => 'Should appear as HTTP call in trace, NOT file op'
];

// 5b. HTTPS URL test — also should route to HTTP call tracking
$start = microtime(true);
$https_content = @file_get_contents('https://httpbin.org/ip');
$results['https_via_fgc'] = [
    'success' => $https_content !== false,
    'bytes' => $https_content !== false ? strlen($https_content) : 0,
    'time_ms' => round((microtime(true) - $start) * 1000, 3),
    'note' => 'HTTPS should also route to HTTP call tracking'
];

// 6. Read non-existent file (error case)
$start = microtime(true);
$bad = @file_get_contents('/nonexistent/file.txt');
$results['read_nonexistent'] = [
    'success' => false,
    'time_ms' => round((microtime(true) - $start) * 1000, 3)
];

// 7. Write to multiple files (batch)
for ($i = 0; $i < 5; $i++) {
    $f = tempnam('/tmp', 'phpray_batch_');
    file_put_contents($f, "Batch write #$i\n");
    file_get_contents($f);
    unlink($f);
}
$results['batch_writes'] = ['count' => 5, 'note' => '5 write+read cycles'];

if (function_exists('phpray_mark')) {
    phpray_mark('file_tests_end');
}

// Summary
$results['phpray'] = [
    'tracing' => function_exists('phpray_is_tracing') ? phpray_is_tracing() : 'N/A',
    'request_id' => function_exists('phpray_request_id') ? phpray_request_id() : 'N/A'
];

echo json_encode($results, JSON_PRETTY_PRINT) . "\n";
