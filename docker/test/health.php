<?php
header('Content-Type: application/json');
echo json_encode(['status' => 'ok', 'phpray' => extension_loaded('phpray')]);
