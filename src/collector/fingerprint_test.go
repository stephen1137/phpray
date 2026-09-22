package main

import (
	"testing"
)

func TestFingerprintSQL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple select with number",
			input:    "SELECT * FROM users WHERE id = 1234",
			expected: "SELECT * FROM users WHERE id = ?",
		},
		{
			name:     "select with string literal",
			input:    "SELECT * FROM users WHERE name = 'John'",
			expected: "SELECT * FROM users WHERE name = ?",
		},
		{
			name:     "insert with values",
			input:    "INSERT INTO posts (title, views) VALUES ('Hello World', 42)",
			expected: "INSERT INTO posts (title, views) VALUES (?, ?)",
		},
		{
			name:     "update with multiple conditions",
			input:    "UPDATE users SET name = 'Jane', age = 30 WHERE id = 5",
			expected: "UPDATE users SET name = ?, age = ? WHERE id = ?",
		},
		{
			name:     "IN clause collapse",
			input:    "SELECT * FROM products WHERE id IN (1, 2, 3, 4, 5)",
			expected: "SELECT * FROM products WHERE id IN (?)",
		},
		{
			name:     "LIMIT and OFFSET",
			input:    "SELECT * FROM posts ORDER BY id LIMIT 10 OFFSET 20",
			expected: "SELECT * FROM posts ORDER BY id LIMIT ? OFFSET ?",
		},
		{
			name:     "escaped quotes",
			input:    "SELECT * FROM users WHERE name = 'O\\'Brien'",
			expected: "SELECT * FROM users WHERE name = ?",
		},
		{
			name:     "double quoted quotes",
			input:    "SELECT * FROM users WHERE name = 'it''s'",
			expected: "SELECT * FROM users WHERE name = ?",
		},
		{
			name:     "backtick identifiers preserved",
			input:    "SELECT `id`, `name` FROM `users` WHERE `id` = 42",
			expected: "SELECT `id`, `name` FROM `users` WHERE `id` = ?",
		},
		{
			name:     "table with number in name preserved",
			input:    "SELECT * FROM wp_options WHERE option_id = 5",
			expected: "SELECT * FROM wp_options WHERE option_id = ?",
		},
		{
			name:     "datetime string",
			input:    "WHERE created_at > '2024-01-01 00:00:00'",
			expected: "WHERE created_at > ?",
		},
		{
			name:     "float number",
			input:    "WHERE price > 19.99",
			expected: "WHERE price > ?",
		},
		{
			name:     "whitespace normalization",
			input:    "SELECT  *  FROM\n\tusers\r\n  WHERE  id = 1",
			expected: "SELECT * FROM users WHERE id = ?",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "wordpress autoload query",
			input:    "SELECT option_name, option_value FROM wp_options WHERE autoload = 'yes'",
			expected: "SELECT option_name, option_value FROM wp_options WHERE autoload = ?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FingerprintSQL(tt.input)
			if got != tt.expected {
				t.Errorf("FingerprintSQL(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.expected)
			}
		})
	}
}
