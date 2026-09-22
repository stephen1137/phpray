package main

import (
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
)

// Schema evolution without a migrations table: every CREATE TABLE in the schema is
// the source of truth. Tables are created if missing; for existing tables (from an
// older collector) any column that is missing is added with ALTER TABLE ... ADD COLUMN,
// and only then the indexes are created (an index on a column the old table lacks
// used to abort startup with "no such column").

var (
	reCreateTable = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\((.*)\)\s*$`)
	reColumnDef   = regexp.MustCompile(`(?i)^(\w+)\s+(\w+.*)$`)
)

var constraintKeywords = map[string]bool{"PRIMARY": true, "UNIQUE": true, "FOREIGN": true, "CHECK": true, "CONSTRAINT": true}

// applySchema executes the schema statement by statement, migrating columns of
// existing tables before any index is created.
func applySchema(db *sql.DB, schema string) error {
	var tables []string
	var indexes []string
	for _, stmt := range strings.Split(schema, ";") {
		stmt = stripSQLComments(stmt)
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if reCreateTable.MatchString(stmt) {
			tables = append(tables, stmt)
		} else {
			indexes = append(indexes, stmt)
		}
	}
	for _, stmt := range tables {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
		if err := ensureColumns(db, stmt); err != nil {
			return err
		}
	}
	for _, stmt := range indexes {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", firstWords(stmt, 4), err)
		}
	}
	return nil
}

// ensureColumns adds the columns declared in a CREATE TABLE statement that the
// existing table does not have yet.
func ensureColumns(db *sql.DB, createStmt string) error {
	m := reCreateTable.FindStringSubmatch(createStmt)
	if m == nil {
		return nil
	}
	table, body := m[1], m[2]
	existing := map[string]bool{}
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("table_info %s: %w", table, err)
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[strings.ToLower(name)] = true
	}
	rows.Close()
	for _, def := range splitColumnDefs(body) {
		cm := reColumnDef.FindStringSubmatch(strings.TrimSpace(def))
		if cm == nil {
			continue
		}
		name := cm[1]
		if constraintKeywords[strings.ToUpper(name)] || existing[strings.ToLower(name)] {
			continue
		}
		colDef := addColumnDef(name, cm[2])
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", table, colDef)); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", table, name, err)
		}
		log.Printf("Schema: added column %s.%s (database from an older collector)", table, name)
	}
	return nil
}

// addColumnDef turns a CREATE TABLE column definition into one that SQLite accepts
// in ADD COLUMN: no PRIMARY KEY / UNIQUE / AUTOINCREMENT, and NOT NULL only with a DEFAULT.
func addColumnDef(name, rest string) string {
	r := " " + rest + " "
	for _, kw := range []string{" PRIMARY KEY ", " AUTOINCREMENT ", " UNIQUE "} {
		r = strings.ReplaceAll(strings.ToUpper(r), kw, " ")
	}
	r = strings.TrimSpace(r)
	if strings.Contains(r, "NOT NULL") && !strings.Contains(r, "DEFAULT") {
		def := "0"
		if strings.HasPrefix(r, "TEXT") || strings.HasPrefix(r, "VARCHAR") || strings.HasPrefix(r, "CHAR") {
			def = "''"
		}
		r += " DEFAULT " + def
	}
	return name + " " + r
}

// splitColumnDefs splits the body of CREATE TABLE on top-level commas.
func splitColumnDefs(body string) []string {
	var out []string
	depth, start := 0, 0
	for i, c := range body {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, body[start:])
	return out
}

func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}
