<?php
/**
 * Fast request — should produce SUMMARY trace level (<200ms).
 * Minimal work, no delay.
 */
header('Content-Type: application/json');
echo json_encode([
    'status' => 'ok',
    'type' => 'fast',
    'pid' => getmypid(),
]);
