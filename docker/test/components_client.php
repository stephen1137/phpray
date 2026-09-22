<?php
/**
 * Client for the function-profile test: fires N requests at components.php and
 * records, per request, the X-PHPRay-ID response header and the JSON body.
 *
 * Usage: php components_client.php <base-url> <n> [url|plain]
 *   url   — alternate between /components.php?prof=1 and /components.php (url mode test)
 *
 * Output (stdout): JSON array of {id, header_id, body, uri}
 */
$base = $argv[1] ?? 'http://127.0.0.1:8080';
$n = (int)($argv[2] ?? 20);
$variant = $argv[3] ?? 'plain';

$out = [];
$fail = 0;
for ($i = 0; $i < $n; $i++) {
    $uri = '/components.php';
    if ($variant === 'url') {
        $uri .= ($i % 2 === 0) ? '?prof=1&i=' . $i : '?i=' . $i;
    }
    $ctx = stream_context_create(['http' => ['timeout' => 30, 'ignore_errors' => true]]);
    $body = @file_get_contents($base . $uri, false, $ctx);
    $headerId = null;
    $status = 0;
    foreach ($http_response_header ?? [] as $h) {
        if (preg_match('#^HTTP/\S+ (\d+)#', $h, $m)) $status = (int)$m[1];
        if (stripos($h, 'X-PHPRay-ID:') === 0) $headerId = trim(substr($h, 12));
    }
    $json = $body !== false ? json_decode($body, true) : null;
    if ($status !== 200 || !is_array($json)) {
        $fail++;
        fwrite(STDERR, "request $i: status=$status body=" . substr((string)$body, 0, 200) . "\n");
        continue;
    }
    $out[] = ['i' => $i, 'uri' => $uri, 'status' => $status, 'header_id' => $headerId, 'body' => $json];
}
echo json_encode(['requests' => $n, 'failed' => $fail, 'results' => $out]) . "\n";
exit($fail > 0 ? 1 : 0);
