<?php
/**
 * Per-directory enable test (Apache + mod_php, scripts/test-perdir.sh).
 * Global php.ini has phpray.enabled=0; this directory's .htaccess enables tracing and
 * the full profile. The trace must contain file I/O data (hook installed in MINIT)
 * and a component from the fake plugin (observer registered in MINIT).
 */
$GLOBALS['cnt'] = [];
function cnt(string $c): void { $GLOBALS['cnt'][$c] = ($GLOBALS['cnt'][$c] ?? 0) + 1; }
require __DIR__ . '/../wp-includes/core.php';
require __DIR__ . '/../wp-content/plugins/a/a.php';
$r = a_fib(12);
$data = file_get_contents(__FILE__);
header('Content-Type: application/json');
echo json_encode([
    'id'       => phpray_request_id(),
    'tracing'  => phpray_is_tracing(),
    'enabled'  => ini_get('phpray.enabled'),
    'mode'     => ini_get('phpray.profile_mode'),
    'expected' => $GLOBALS['cnt'],
    'len'      => strlen($data),
    'r'        => $r,
]) . "\n";
