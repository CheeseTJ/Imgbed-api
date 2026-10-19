package store

import "testing"

// TestSplitStatements 验证内嵌 DDL 能被正确切分。
//
// MySQL 驱动不允许一次发送多条语句，因此这段切分逻辑是建表能否成功的关键。
func TestSplitStatements(t *testing.T) {
	stmts := splitStatements(schemaSQL)

	if len(stmts) == 0 {
		t.Fatal("切分结果为空，schema.sql 可能未嵌入")
	}

	// 每张表一条 CREATE TABLE
	tables := map[string]bool{"users": false, "folders": false, "images": false, "jobs": false}
	for _, s := range stmts {
		if len(s) == 0 {
			t.Fatal("出现空语句")
		}
		if s[len(s)-1] == ';' {
			t.Errorf("语句残留分号: %.60s", s)
		}
		for name := range tables {
			if containsCreateTable(s, name) {
				tables[name] = true
			}
		}
	}

	for name, found := range tables {
		if !found {
			t.Errorf("schema.sql 中缺少 %s 表的建表语句", name)
		}
	}
}

// TestSplitStatementsStripsComments 确认注释行不会被送进数据库。
func TestSplitStatementsStripsComments(t *testing.T) {
	script := "-- 这是注释\nCREATE TABLE a (id INT);\n-- 另一段注释\nCREATE TABLE b (id INT);\n"
	stmts := splitStatements(script)

	if len(stmts) != 2 {
		t.Fatalf("期望 2 条语句，实际 %d 条: %v", len(stmts), stmts)
	}
	for _, s := range stmts {
		if len(s) >= 2 && s[:2] == "--" {
			t.Errorf("注释被当成了语句: %q", s)
		}
	}
}

func containsCreateTable(stmt, table string) bool {
	needle := "CREATE TABLE IF NOT EXISTS " + table
	return len(stmt) >= len(needle) && contains(stmt, needle)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
