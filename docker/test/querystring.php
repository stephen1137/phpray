<?php
/**
 * PHPRay URI fingerprinting test — test with query strings.
 */
header('Content-Type: application/json');

echo json_encode([
    'status' => 'ok',
    'type' => 'querystring_test',
    'request_uri' => $_SERVER['REQUEST_URI'] ?? 'unknown',
    'tracing' => phpray_is_tracing(),
    'request_id' => phpray_request_id(),
]) . "\n";
