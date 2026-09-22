--TEST--
phpray.master_switch=0 keeps the extension completely inert (h1 incident 2026-09-20)
--EXTENSIONS--
phpray
--INI--
phpray.master_switch=0
phpray.enabled=1
phpray.trace_cli=1
phpray.mode=all
phpray.output_mode=file
phpray.output_path=/tmp/phpray-phpt-off.jsonl
--FILE--
<?php
@unlink('/tmp/phpray-phpt-off.jsonl');
function work($n) { $s = 0; for ($i = 0; $i < $n; $i++) { $s += strlen(md5((string) $i)); } return $s; }
var_dump(work(50) === 1600);
var_dump(extension_loaded('phpray'));
// Nothing may be traced: no observer, no handler replacement, no request.
var_dump(phpray_is_tracing());
var_dump(phpray_request_id());
?>
--EXPECT--
bool(true)
bool(true)
bool(false)
bool(false)
