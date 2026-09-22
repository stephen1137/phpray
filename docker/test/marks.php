<?php
/**
 * PHPRay marks test — exercises phpray_mark() userland function.
 */
header('Content-Type: application/json');

$result = [];

// Check if tracing
$result['tracing'] = phpray_is_tracing();
$result['request_id'] = phpray_request_id();

// Mark: start of DB simulation
phpray_mark("db_start");
usleep(50000); // 50ms simulated DB query
phpray_mark("db_end");

// Mark: start of template rendering
phpray_mark("template_start");
$html = '';
for ($i = 0; $i < 5000; $i++) {
    $html .= md5(random_bytes(16));
}
phpray_mark("template_end");

// Mark: start of external API call
phpray_mark("api_start");
usleep(100000); // 100ms simulated API call
phpray_mark("api_end");

$result['status'] = 'ok';
$result['marks'] = 6;

echo json_encode($result, JSON_PRETTY_PRINT) . "\n";
