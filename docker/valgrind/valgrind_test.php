<?php
/**
 * PHPRay Valgrind Memory Leak Test
 * 
 * Exercises all PHPRay features to detect memory leaks:
 * - Request tracing (URI, method, timing)
 * - phpray_mark() custom marks
 * - PHP error capture
 * - PDO/SQLite queries (triggers query hooking)
 * - file_get_contents / file_put_contents hooks
 * - Ring buffer writing
 * - phpray_ring_stats() / phpray_is_tracing() / phpray_request_id()
 *
 * Run under Valgrind to detect leaks in PHPRay code.
 */

// Iteration count — each iteration simulates one "request" worth of work.
// In CLI mode, it's all one request, but we repeat operations to amplify leaks.
$iterations = (int)($argv[1] ?? 50);

echo "PHPRay Valgrind Memory Leak Test\n";
echo "=================================\n";
echo "Iterations: {$iterations}\n";
echo "Extension loaded: " . (extension_loaded('phpray') ? 'YES' : 'NO') . "\n";
echo "Tracing: " . (phpray_is_tracing() ? 'YES' : 'NO') . "\n";
echo "Request ID: " . (phpray_request_id() ?: 'N/A') . "\n\n";

// ---- Test 1: phpray_mark() — repeated marks ----
echo "Test 1: phpray_mark() x{$iterations}...\n";
for ($i = 0; $i < $iterations; $i++) {
    phpray_mark("test_mark_{$i}");
}
echo "  Marks placed: {$iterations} (max " . min($iterations, 50) . " captured)\n";

// ---- Test 2: PHP error capture ----
echo "Test 2: Error capture...\n";
$old_level = error_reporting(E_ALL);
for ($i = 0; $i < min($iterations, 20); $i++) {
    @trigger_error("Test warning {$i}", E_USER_WARNING);
}
error_reporting($old_level);
echo "  Errors triggered: " . min($iterations, 20) . "\n";

// ---- Test 3: PDO/SQLite queries (exercises query hooks) ----
echo "Test 3: PDO/SQLite queries...\n";
$dbFile = '/tmp/valgrind_test.db';
try {
    $pdo = new PDO("sqlite:{$dbFile}");
    $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
    
    $pdo->exec("CREATE TABLE IF NOT EXISTS test (id INTEGER PRIMARY KEY, val TEXT, num REAL)");
    $pdo->exec("DELETE FROM test");
    
    // INSERT via exec
    for ($i = 0; $i < min($iterations, 30); $i++) {
        $pdo->exec("INSERT INTO test (val, num) VALUES ('val_{$i}', {$i}.5)");
    }
    
    // SELECT via query
    for ($i = 0; $i < min($iterations, 20); $i++) {
        $result = $pdo->query("SELECT * FROM test WHERE num > {$i}");
        $rows = $result->fetchAll(PDO::FETCH_ASSOC);
    }
    
    // Prepared statement via execute
    $stmt = $pdo->prepare("SELECT * FROM test WHERE val = ?");
    for ($i = 0; $i < min($iterations, 20); $i++) {
        $stmt->execute(["val_{$i}"]);
        $row = $stmt->fetch(PDO::FETCH_ASSOC);
    }
    
    $count = $pdo->query("SELECT COUNT(*) FROM test")->fetchColumn();
    echo "  Queries executed, {$count} rows in table\n";
    
    $pdo = null; // Close connection
} catch (PDOException $e) {
    echo "  PDO Error: {$e->getMessage()}\n";
}

// ---- Test 4: file_get_contents / file_put_contents hooks ----
echo "Test 4: File I/O hooks...\n";
$tmpFile = '/tmp/valgrind_filetest.txt';
for ($i = 0; $i < min($iterations, 30); $i++) {
    $data = str_repeat("PHPRay test data line {$i}\n", 10);
    file_put_contents($tmpFile, $data);
    $content = file_get_contents($tmpFile);
}
@unlink($tmpFile);
echo "  File ops: " . (min($iterations, 30) * 2) . " (read+write cycles)\n";

// ---- Test 5: file_get_contents with URL (HTTP hook path) ----
echo "Test 5: URL detection in file_get_contents...\n";
// We don't actually make HTTP calls (no server), but we can test the
// URL detection path by calling with a URL that will fail gracefully
$urlAttempts = 0;
for ($i = 0; $i < min($iterations, 5); $i++) {
    // Suppress the error — we just want to exercise the URL detection code path
    @file_get_contents("http://127.0.0.1:1/nonexistent_{$i}");
    $urlAttempts++;
}
echo "  URL attempts: {$urlAttempts} (expected to fail, testing hook path)\n";

// ---- Test 6: Ring buffer stats ----
echo "Test 6: Ring buffer stats...\n";
$stats = phpray_ring_stats();
if ($stats) {
    echo "  Records: {$stats['records']}\n";
    echo "  Drops: {$stats['drops']}\n";
    echo "  Fill: " . round($stats['fill_pct'], 1) . "%\n";
    echo "  Capacity: " . round($stats['capacity'] / 1024) . " KB\n";
} else {
    echo "  Ring buffer not active (file output mode)\n";
}

// ---- Test 7: Repeated phpray_is_tracing() and phpray_request_id() calls ----
echo "Test 7: Userland function stress...\n";
for ($i = 0; $i < $iterations; $i++) {
    $tracing = phpray_is_tracing();
    $rid = phpray_request_id();
    $stats = phpray_ring_stats();
}
echo "  Called phpray_is_tracing/request_id/ring_stats x{$iterations}\n";

// ---- Summary ----
echo "\n=== Memory Report ===\n";
echo "Peak memory: " . round(memory_get_peak_usage(true) / 1024 / 1024, 2) . " MB\n";
echo "Current memory: " . round(memory_get_usage(true) / 1024 / 1024, 2) . " MB\n";
echo "\nTest complete. Check Valgrind output for PHPRay-specific leaks.\n";

// Cleanup
@unlink($dbFile);
