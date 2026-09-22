<?php
/**
 * PHPRay Test Script — simulates realistic WordPress-like request load.
 * 
 * Includes: CPU work, random delay, memory allocation, file I/O.
 */

// Random delay simulating DB queries + external calls (50-500ms)
usleep(rand(50000, 500000));

// CPU work — simulate PHP template rendering + plugin processing
$start = microtime(true);
$hashes = [];
for ($i = 0; $i < 10000; $i++) {
    $hashes[] = md5(random_bytes(32));
}
$cpu_time = microtime(true) - $start;

// Memory allocation — simulate WP object cache
$cache = [];
for ($i = 0; $i < 100; $i++) {
    $cache["key_$i"] = str_repeat("x", rand(100, 1000));
}

// File I/O — simulate reading config/template files
$file_data = file_get_contents(__FILE__);

// Occasional slow request (simulate N+1 or external API)
if (rand(1, 20) === 1) {
    usleep(rand(1000000, 2000000)); // 1-2s extra delay
}

// Occasional very slow request (should trigger ALERT level)
if (rand(1, 100) === 1) {
    usleep(3500000); // 3.5s — above alert threshold
}

header('Content-Type: application/json');
echo json_encode([
    'status' => 'ok',
    'cpu_time' => round($cpu_time, 4),
    'memory_peak' => memory_get_peak_usage(true),
    'memory_peak_mb' => round(memory_get_peak_usage(true) / 1024 / 1024, 2),
    'pid' => getmypid(),
    'time' => microtime(true),
]);
