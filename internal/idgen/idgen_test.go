package idgen

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// TestNewFormat 验证 ULID 的长度、字符集与可排序性。
func TestNewFormat(t *testing.T) {
	id := New()
	if len(id) != 26 {
		t.Fatalf("ULID 长度应为 26，实际 %d: %q", len(id), id)
	}
	for _, ch := range id {
		if !strings.ContainsRune(crockford, ch) {
			t.Fatalf("ULID 含非法字符 %q: %q", ch, id)
		}
	}
}

// TestNewUniqueAndOrdered 同一毫秒内连续生成也必须不重复且单调递增。
func TestNewUniqueAndOrdered(t *testing.T) {
	const n = 5000

	ids := make([]string, 0, n)
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := New()
		if seen[id] {
			t.Fatalf("第 %d 次生成出现重复 ID: %s", i, id)
		}
		seen[id] = true
		ids = append(ids, id)
	}

	// ULID 的字典序等价于生成顺序
	if !sort.StringsAreSorted(ids) {
		t.Fatal("同一批次内 ULID 未保持单调递增")
	}
}

// TestTimestampPrefix 验证时间戳部分按毫秒推进。
func TestTimestampPrefix(t *testing.T) {
	before := New()
	time.Sleep(3 * time.Millisecond)
	after := New()

	// 前 10 个字符编码 48 位毫秒时间戳，时间前进后字典序应变大
	if before >= after {
		t.Fatalf("时间前进后 ULID 未变大: %s >= %s", before, after)
	}
}
