<?php
// Sits in the docroot (no .htaccess override): with phpray.enabled=0 globally this
// request must not be traced at all (no X-PHPRay-ID header, no JSONL record).
header('Content-Type: application/json');
echo json_encode(['tracing' => phpray_is_tracing(), 'enabled' => ini_get('phpray.enabled'), 'id' => phpray_request_id()]) . "\n";
