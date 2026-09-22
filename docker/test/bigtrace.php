<?php
/**
 * Produces a large trace record (~8-10 KB: 50 marks with long names + 20 warnings with
 * long messages + a component) for the JSONL concurrency test (scripts/test-jsonl-concurrency.sh).
 */
$GLOBALS['cnt'] = [];
function cnt(string $c): void {}
require __DIR__ . '/wp-content/plugins/a/a.php';
for ($i = 0; $i < 50; $i++) {
    phpray_mark(str_pad("mark_{$i}_", 60, 'x'));
}
$old = error_reporting(E_ALL);
for ($i = 0; $i < 20; $i++) {
    @trigger_error(str_pad("warning number {$i} ", 250, 'y'), E_USER_WARNING);
}
error_reporting($old);
a_fib(10);
header('Content-Type: text/plain');
echo phpray_request_id() ?: 'none', "\n";
