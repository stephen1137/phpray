<?php
/**
 * Slow request — should produce FULL or ALERT trace level.
 * Simulates heavy processing.
 */
usleep(2000000); // 2 seconds

// Heavy CPU work
for ($i = 0; $i < 50000; $i++) {
    md5(random_bytes(64));
}

header('Content-Type: application/json');
echo json_encode([
    'status' => 'ok',
    'type' => 'slow',
    'pid' => getmypid(),
    'memory_peak_mb' => round(memory_get_peak_usage(true) / 1024 / 1024, 2),
]);
