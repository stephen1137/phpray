<?php
/**
 * PHPRay curl_multi test — tests async HTTP call tracking.
 * 
 * Tests: curl_multi_add_handle, curl_multi_exec, curl_multi_info_read, curl_multi_remove_handle
 */

header('Content-Type: text/plain');

echo "=== PHPRay curl_multi Test ===\n\n";

// Mark the start
if (function_exists('phpray_mark')) {
    phpray_mark('curl_multi_test_start');
}

// Create multi handle
$mh = curl_multi_init();

// Create 3 easy handles with different URLs
$urls = [
    'http://httpbin.org/get',
    'http://httpbin.org/status/200',
    'http://httpbin.org/delay/1',
];

$handles = [];
foreach ($urls as $i => $url) {
    $ch = curl_init();
    curl_setopt($ch, CURLOPT_URL, $url);
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    curl_setopt($ch, CURLOPT_TIMEOUT, 5);
    curl_multi_add_handle($mh, $ch);
    $handles[$i] = $ch;
    echo "Added handle $i: $url\n";
}

// Execute multi handle
$running = null;
do {
    $status = curl_multi_exec($mh, $running);
    if ($running) {
        curl_multi_select($mh, 1.0);
    }
} while ($running > 0 && $status == CURLM_OK);

echo "\nAll transfers complete.\n\n";

// Read info for each completed transfer
$completed = 0;
while ($info = curl_multi_info_read($mh)) {
    $completed++;
    echo "Info read #$completed: msg={$info['msg']}, result={$info['result']}\n";
}
echo "Total info reads: $completed\n\n";

// Remove and close handles
foreach ($handles as $i => $ch) {
    $code = curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
    echo "Handle $i: HTTP $code\n";
    curl_multi_remove_handle($mh, $ch);
    curl_close($ch);
}

curl_multi_close($mh);

// Mark the end
if (function_exists('phpray_mark')) {
    phpray_mark('curl_multi_test_end');
}

echo "\n=== Test complete ===\n";
echo "If PHPRay is working, the trace should show 3 HTTP calls from curl_multi.\n";

// Also test: local URLs that don't use external services
echo "\n=== Local curl_multi test (reliable) ===\n";

$mh2 = curl_multi_init();
$local_handles = [];

// Use PHP's built-in server for local requests
for ($i = 0; $i < 3; $i++) {
    $ch = curl_init();
    curl_setopt($ch, CURLOPT_URL, "http://localhost:8080/info.php");
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    curl_setopt($ch, CURLOPT_TIMEOUT, 2);
    curl_multi_add_handle($mh2, $ch);
    $local_handles[] = $ch;
}

$running = null;
do {
    $status = curl_multi_exec($mh2, $running);
    if ($running) {
        curl_multi_select($mh2, 0.5);
    }
} while ($running > 0 && $status == CURLM_OK);

// Read completions via info_read
while ($info = curl_multi_info_read($mh2)) {
    // info_read triggers recording in PHPRay
}

foreach ($local_handles as $ch) {
    $code = curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
    echo "Local handle: HTTP $code\n";
    curl_multi_remove_handle($mh2, $ch);
    curl_close($ch);
}

curl_multi_close($mh2);

echo "\n=== All curl_multi tests done ===\n";

if (function_exists('phpray_is_tracing') && phpray_is_tracing()) {
    echo "PHPRay is tracing this request (ID: " . phpray_request_id() . ")\n";
    echo "Expected: 6 HTTP calls from curl_multi (3 external + 3 local)\n";
    echo "Plus any regular curl_exec calls would be separate.\n";
}
