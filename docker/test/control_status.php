<?php
// Control table as seen by this worker + whether this request is profiled (scripts/test-control.sh)
$GLOBALS['cnt'] = [];
function cnt(string $c): void {}
require __DIR__ . '/wp-content/plugins/a/a.php';
a_fib(8);
header('Content-Type: application/json');
echo json_encode(['id' => phpray_request_id(), 'control' => phpray_control_status(), 'uri' => $_SERVER['REQUEST_URI'] ?? null]) . "\n";
