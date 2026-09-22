package main

import (
	"strings"
	"unicode"
)

// FingerprintSQL normalizes a SQL query by replacing literal values with placeholders.
// This allows grouping identical query patterns together for analysis.
//
// Examples:
//   SELECT * FROM users WHERE id = 1234        → SELECT * FROM users WHERE id = ?
//   INSERT INTO posts VALUES ('hello', 42)     → INSERT INTO posts VALUES (?, ?)
//   WHERE name IN ('a', 'b', 'c')              → WHERE name IN (?)
//   LIMIT 10 OFFSET 20                         → LIMIT ? OFFSET ?
//   WHERE created_at > '2024-01-01 00:00:00'   → WHERE created_at > ?
func FingerprintSQL(sql string) string {
	if len(sql) == 0 {
		return sql
	}

	var b strings.Builder
	b.Grow(len(sql))

	i := 0
	n := len(sql)

	for i < n {
		ch := sql[i]

		// Skip whitespace runs (normalize to single space)
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			b.WriteByte(' ')
			for i < n && (sql[i] == ' ' || sql[i] == '\t' || sql[i] == '\n' || sql[i] == '\r') {
				i++
			}
			continue
		}

		// Single-quoted strings → ?
		if ch == '\'' {
			b.WriteByte('?')
			i++ // skip opening quote
			for i < n {
				if sql[i] == '\'' {
					i++ // skip closing quote
					// Handle escaped quotes ''
					if i < n && sql[i] == '\'' {
						i++
						continue
					}
					break
				}
				if sql[i] == '\\' && i+1 < n {
					i += 2 // skip escape sequence
					continue
				}
				i++
			}
			continue
		}

		// Double-quoted identifiers — keep as-is (MySQL column names)
		if ch == '"' {
			b.WriteByte(ch)
			i++
			for i < n && sql[i] != '"' {
				b.WriteByte(sql[i])
				i++
			}
			if i < n {
				b.WriteByte(sql[i])
				i++
			}
			continue
		}

		// Backtick-quoted identifiers — keep as-is
		if ch == '`' {
			b.WriteByte(ch)
			i++
			for i < n && sql[i] != '`' {
				b.WriteByte(sql[i])
				i++
			}
			if i < n {
				b.WriteByte(sql[i])
				i++
			}
			continue
		}

		// Numbers (integers and floats) → ?
		if (ch >= '0' && ch <= '9') || (ch == '.' && i+1 < n && sql[i+1] >= '0' && sql[i+1] <= '9') {
			// Check if this is part of an identifier (e.g., table_1, col2)
			if i > 0 && (sql[i-1] == '_' || isIdentChar(sql[i-1])) {
				// Part of identifier — keep
				b.WriteByte(ch)
				i++
				continue
			}
			b.WriteByte('?')
			// Skip the entire number
			for i < n && (sql[i] >= '0' && sql[i] <= '9' || sql[i] == '.' || sql[i] == 'e' || sql[i] == 'E' || sql[i] == '+' || sql[i] == '-') {
				i++
			}
			continue
		}

		// Hex numbers: 0x... → ?
		if ch == '0' && i+1 < n && (sql[i+1] == 'x' || sql[i+1] == 'X') {
			b.WriteByte('?')
			i += 2
			for i < n && isHexDigit(sql[i]) {
				i++
			}
			continue
		}

		// Collapse IN (?, ?, ?, ...) → IN (?)
		if ch == '(' {
			// Check if previous word is IN
			trimmed := strings.TrimRight(b.String(), " ")
			upperTrimmed := strings.ToUpper(trimmed)
			if strings.HasSuffix(upperTrimmed, "IN") {
				b.WriteByte('(')
				i++
				// Skip everything until closing paren
				depth := 1
				for i < n && depth > 0 {
					if sql[i] == '(' {
						depth++
					} else if sql[i] == ')' {
						depth--
					}
					i++
				}
				b.WriteString("?)")
				continue
			}
		}

		// Everything else — copy as-is (but lowercase keywords for normalization)
		b.WriteByte(ch)
		i++
	}

	result := b.String()

	// Normalize multiple spaces
	for strings.Contains(result, "  ") {
		result = strings.ReplaceAll(result, "  ", " ")
	}

	return strings.TrimSpace(result)
}

func isIdentChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isHexDigit(ch byte) bool {
	return (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}

// NormalizeSQL lowercases SQL keywords while preserving identifiers.
// This is a simpler normalization that just lowercases everything
// (good enough for fingerprinting since MySQL is case-insensitive for keywords).
func NormalizeSQL(sql string) string {
	// Simple approach: lowercase the entire thing
	// This works because we're comparing fingerprints, not executing SQL
	var b strings.Builder
	b.Grow(len(sql))

	inQuote := false
	quoteChar := byte(0)

	for i := 0; i < len(sql); i++ {
		ch := sql[i]

		if inQuote {
			b.WriteByte(ch)
			if ch == quoteChar {
				inQuote = false
			}
			continue
		}

		if ch == '\'' || ch == '"' || ch == '`' {
			inQuote = true
			quoteChar = ch
			b.WriteByte(ch)
			continue
		}

		b.WriteRune(unicode.ToLower(rune(ch)))
	}

	return b.String()
}
