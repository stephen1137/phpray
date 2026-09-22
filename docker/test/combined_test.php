<?php
/**
 * PHPRay combined test — simulates realistic WordPress-like request with DB + curl + marks.
 * Tests all hooks together.
 */
header('Content-Type: application/json');

$result = ['status' => 'ok', 'type' => 'combined'];

// Phase 1: Database queries
phpray_mark("db_init");

$pdo = new PDO('sqlite::memory:');
$pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);

$pdo->exec("CREATE TABLE posts (id INTEGER PRIMARY KEY, title TEXT, content TEXT, created_at TEXT)");

// Simulate WordPress loading posts
for ($i = 1; $i <= 20; $i++) {
    $stmt = $pdo->prepare("INSERT INTO posts (title, content, created_at) VALUES (?, ?, ?)");
    $stmt->execute(["Post $i", str_repeat("Content of post $i. ", 50), date('Y-m-d H:i:s')]);
}

// N+1 query simulation (common WordPress antipattern)
$posts = $pdo->query("SELECT * FROM posts")->fetchAll(PDO::FETCH_ASSOC);
foreach ($posts as $post) {
    $pdo->query("SELECT * FROM posts WHERE id = " . $post['id']);
}

phpray_mark("db_done");

// Phase 2: External HTTP call
phpray_mark("http_start");
$ch = curl_init();
curl_setopt($ch, CURLOPT_URL, 'http://localhost:8080/fast.php');
curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
curl_setopt($ch, CURLOPT_TIMEOUT, 5);
curl_exec($ch);
curl_close($ch);
phpray_mark("http_done");

// Phase 3: Template rendering
phpray_mark("render_start");
usleep(100000); // 100ms delay
for ($i = 0; $i < 3000; $i++) {
    md5(random_bytes(16));
}
phpray_mark("render_done");

// Summary
$result['queries'] = 42; // 1 create + 20 inserts + 1 select all + 20 individual selects
$result['curl_calls'] = 1;
$result['tracing'] = phpray_is_tracing();
$result['request_id'] = phpray_request_id();

echo json_encode($result, JSON_PRETTY_PRINT) . "\n";
