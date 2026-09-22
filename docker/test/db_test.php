<?php
/**
 * PHPRay database hook test — uses PDO with SQLite (no MySQL server needed).
 * Tests: PDO::query, PDO::exec, PDOStatement::execute (prepared statements).
 */
header('Content-Type: application/json');

$result = ['status' => 'ok', 'type' => 'db_test'];

try {
    // Create in-memory SQLite database
    $pdo = new PDO('sqlite::memory:');
    $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
    
    // PDO::exec — CREATE TABLE
    $pdo->exec("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, email TEXT)");
    
    // PDO::exec — INSERT
    $affected = $pdo->exec("INSERT INTO users (name, email) VALUES ('John', 'john@test.com')");
    $pdo->exec("INSERT INTO users (name, email) VALUES ('Jane', 'jane@test.com')");
    $pdo->exec("INSERT INTO users (name, email) VALUES ('Bob', 'bob@test.com')");
    
    // PDO::query — SELECT
    $stmt = $pdo->query("SELECT * FROM users WHERE id > 0");
    $users = $stmt->fetchAll(PDO::FETCH_ASSOC);
    
    // PDOStatement::execute — prepared statement
    $stmt = $pdo->prepare("SELECT * FROM users WHERE name = ?");
    $stmt->execute(['John']);
    $john = $stmt->fetch(PDO::FETCH_ASSOC);
    
    // Another prepared statement
    $stmt = $pdo->prepare("UPDATE users SET email = :email WHERE name = :name");
    $stmt->execute([':email' => 'john.new@test.com', ':name' => 'John']);
    
    // Simulate slow query with many operations
    for ($i = 0; $i < 10; $i++) {
        $pdo->exec("INSERT INTO users (name, email) VALUES ('user$i', 'user$i@test.com')");
    }
    
    // Final count
    $stmt = $pdo->query("SELECT COUNT(*) as cnt FROM users");
    $count = $stmt->fetch(PDO::FETCH_ASSOC);
    
    $result['user_count'] = (int)$count['cnt'];
    $result['tracing'] = phpray_is_tracing();
    $result['request_id'] = phpray_request_id();
    
} catch (PDOException $e) {
    $result['error'] = $e->getMessage();
}

echo json_encode($result, JSON_PRETTY_PRINT) . "\n";
