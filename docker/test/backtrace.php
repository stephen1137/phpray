<?php
/**
 * PHPRay backtrace capture test.
 * Simulates slow queries and HTTP calls to trigger backtrace capture.
 */

header('Content-Type: text/plain');

// Helper: simulate a slow database query (uses SQLite for testing)
function slow_database_call() {
    $db = new PDO('sqlite:/tmp/bt_test.db');
    $db->exec('CREATE TABLE IF NOT EXISTS slow_test (id INTEGER, data TEXT)');
    
    // Insert lots of rows to make the query slow
    $db->beginTransaction();
    for ($i = 0; $i < 1000; $i++) {
        $db->exec("INSERT INTO slow_test VALUES ($i, '" . str_repeat('x', 1000) . "')");
    }
    $db->commit();
    
    // Do a complex query that should take some time
    $stmt = $db->query("SELECT COUNT(*), GROUP_CONCAT(data) FROM slow_test WHERE id < 500");
    $result = $stmt->fetchAll();
    
    // Clean up
    $db->exec('DROP TABLE slow_test');
    return count($result);
}

function outer_function() {
    return inner_function();
}

function inner_function() {
    return slow_database_call();
}

// Force full trace level (request will be >200ms with this work)
echo "PHPRay Backtrace Test\n";
echo "=====================\n\n";

$start = microtime(true);

// Call through nested functions to test backtrace depth
$result = outer_function();

// Simulate slow curl call
if (function_exists('curl_exec')) {
    // This should be slow enough to trigger HTTP backtrace capture
    $ch = curl_init('http://httpbin.org/delay/1');
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    curl_setopt($ch, CURLOPT_TIMEOUT, 2);
    curl_exec($ch);
    curl_close($ch);
}

$elapsed = (microtime(true) - $start) * 1000;

echo "Query result rows: $result\n";
echo sprintf("Elapsed: %.1fms\n", $elapsed);
echo "\nCheck /tmp/phpray.jsonl for backtrace data (look for 'bt' field in queries/http_calls)\n";

if (function_exists('phpray_is_tracing')) {
    echo "Tracing: " . (phpray_is_tracing() ? 'yes' : 'no') . "\n";
    echo "Request ID: " . (phpray_request_id() ?: 'N/A') . "\n";
}
