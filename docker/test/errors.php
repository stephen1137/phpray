<?php
/**
 * PHPRay error capture test — generates various PHP errors/warnings.
 */
header('Content-Type: application/json');

// Generate some warnings/notices
@$undefined_var;                      // Notice: undefined variable (suppressed)
$arr = [1, 2, 3];
trigger_error("Custom user warning", E_USER_WARNING);
trigger_error("Custom user notice", E_USER_NOTICE);

// Deprecated usage
trigger_error("This function is deprecated", E_USER_DEPRECATED);

echo json_encode([
    'status' => 'ok',
    'type' => 'errors_test',
    'tracing' => phpray_is_tracing(),
    'request_id' => phpray_request_id(),
]) . "\n";
