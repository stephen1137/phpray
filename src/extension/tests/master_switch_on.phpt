--TEST--
phpray.master_switch=1 (default) leaves tracing working
--EXTENSIONS--
phpray
--INI--
phpray.master_switch=1
phpray.enabled=1
phpray.trace_cli=1
phpray.mode=all
phpray.output_mode=file
phpray.output_path=/tmp/phpray-phpt-on.jsonl
--FILE--
<?php
var_dump(phpray_is_tracing());
var_dump(phpray_request_id() !== '' && phpray_request_id() !== null);
?>
--EXPECT--
bool(true)
bool(true)
