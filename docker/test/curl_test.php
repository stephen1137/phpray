<?php
/**
 * PHPRay curl hook test — makes real HTTP calls to localhost.
 */
header('Content-Type: application/json');

$result = ['status' => 'ok', 'type' => 'curl_test'];

// Call 1: curl to localhost health endpoint
$ch = curl_init();
curl_setopt($ch, CURLOPT_URL, 'http://localhost:8080/health.php');
curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
curl_setopt($ch, CURLOPT_TIMEOUT, 5);
$response1 = curl_exec($ch);
$code1 = curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
curl_close($ch);

// Call 2: curl to localhost fast endpoint
$ch = curl_init();
curl_setopt($ch, CURLOPT_URL, 'http://localhost:8080/fast.php');
curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
curl_setopt($ch, CURLOPT_TIMEOUT, 5);
$response2 = curl_exec($ch);
$code2 = curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
curl_close($ch);

$result['curl_calls'] = 2;
$result['response_codes'] = [$code1, $code2];
$result['tracing'] = phpray_is_tracing();
$result['request_id'] = phpray_request_id();

echo json_encode($result, JSON_PRETTY_PRINT) . "\n";
