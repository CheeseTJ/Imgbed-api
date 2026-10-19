package api

import (
	"strings"
	"testing"
)

// TestSanitizeSegmentBlocksTraversal 是本文件最重要的一条。
//
// ownerID 与目录名都会进入 R2 对象 key。如果把它们原样拼进去，
// 一个叫 `../../` 的目录名就能让写入落到 bucket 里的预期之外的位置。
func TestSanitizeSegmentBlocksTraversal(t *testing.T) {
	cases := []string{
		"../../../etc/passwd",
		"..",
		".",
		"./../secret",
		"a/../../../b",
		"\x00null",
		"",
	}

	for _, in := range cases {
		got := sanitizeSegment(in)
		if got == "" {
			t.Errorf("%q 清洗后为空，应回退为占位符", in)
		}
		if strings.Contains(got, "/") {
			t.Errorf("%q 清洗后仍含斜杠：%q", in, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("%q 清洗后仍含 ..：%q", in, got)
		}
		if got != "." && strings.HasPrefix(got, ".") {
			t.Errorf("%q 清洗后以点开头：%q", in, got)
		}
	}
}

func TestSanitizeSegmentKeepsSafeChars(t *testing.T) {
	if got := sanitizeSegment("uc_8f2a-A1.b9"); got != "uc_8f2a-A1.b9" {
		t.Errorf("安全字符被误改：%q", got)
	}
}

// TestSanitizeSegmentKeepsCJK 锁定「中文目录名不被压成下划线」这条性质。
//
// 早先的实现把非 [a-zA-Z0-9_.-] 一律替换成下划线，于是「风景」和「截图」
// 都塌缩成 `__`——两个不同目录在 R2 里共用一个前缀。
// 而 SPEC 4.3 选方案 A（key 里编码路径）的唯一理由就是控制台里能看懂目录结构，
// 塌缩之后这个理由当场作废。SPEC 自己给的示例 key 也正是 `u/uc_8f2a/风景/2026/...`。
func TestSanitizeSegmentKeepsCJK(t *testing.T) {
	if got := sanitizeSegment("风景"); got != "风景" {
		t.Errorf("中文目录名应原样保留，实际 %q", got)
	}

	// 这条才是真正防回归的：两个不同中文目录必须落在不同前缀下
	a := objectKey("uc_1", "风景", "id1", ".jpg")
	b := objectKey("uc_1", "截图", "id2", ".jpg")
	if !strings.Contains(a, "风景") {
		t.Errorf("key 中应保留中文目录名：%q", a)
	}
	if strings.Contains(b, "风景") {
		t.Errorf("不同目录被混为一谈：%q", b)
	}
}

// TestSanitizeSegmentReplacesProblematic 确认「危险/别扭」的字符仍被替换。
//
// 放宽不等于放行：斜杠会破坏层级、空格与引号在 URL 与命令行里都需要转义、
// % 会和百分号编码混淆。这些没有一个是「想保留」的。
func TestSanitizeSegmentReplacesProblematic(t *testing.T) {
	cases := map[string]string{
		"a b":  "a_b", // 空格
		"a\\b": "a_b", // 反斜杠
		"a\tb": "a_b", // 制表符
		"a:b":  "a_b", // 冒号
		"a?b":  "a_b", // 查询串分隔符
		"a#b":  "a_b", // 片段分隔符
		"a%b":  "a_b", // 百分号会与 URL 编码混淆
		`a"b`:  "a_b",
		"a|b":  "a_b",
		"a*b":  "a_b",
		"a<b":  "a_b",
	}
	for in, want := range cases {
		if got := sanitizeSegment(in); got != want {
			t.Errorf("sanitizeSegment(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestObjectKeyLayout 锁定 key 的形态：u/{owner}/{...}/{id}{ext}
func TestObjectKeyLayout(t *testing.T) {
	got := objectKey("uc_123", "", "01H8XK", ".jpg")
	if got != "u/uc_123/01H8XK.jpg" {
		t.Errorf("根目录 key = %q", got)
	}

	// 目录路径中的空段应被丢弃，避免出现 u/a//b 这种重复斜杠
	got = objectKey("uc_123", "trip//2026/", "01H8XK", ".png")
	if strings.Contains(got, "//") {
		t.Errorf("key 中出现重复斜杠：%q", got)
	}
	if !strings.HasPrefix(got, "u/uc_123/") || !strings.HasSuffix(got, "01H8XK.png") {
		t.Errorf("key 形态不对：%q", got)
	}
}

// TestObjectKeySanitizesOwner 确认 owner 段被清洗。
func TestObjectKeySanitizesOwner(t *testing.T) {
	got := objectKey("../../evil", "", "id1", ".png")
	if strings.Contains(got, "..") {
		t.Fatalf("owner 未清洗：%q", got)
	}
	if !strings.HasPrefix(got, "u/") {
		t.Fatalf("key 前缀不对：%q", got)
	}
}

// TestCleanFilename 覆盖展示名的边界情况。
func TestCleanFilename(t *testing.T) {
	if got := cleanFilename("holiday.jpg", "id1", ".jpg"); got != "holiday.jpg" {
		t.Errorf("正常文件名被改动：%q", got)
	}
	// 路径形式的文件名只取最后一段，防止把目录信息带进展示名
	if got := cleanFilename("a/b/c.png", "id1", ".png"); got != "c.png" {
		t.Errorf("路径应被剥掉，实际 %q", got)
	}
	if got := cleanFilename("   ", "id1", ".png"); got != "id1.png" {
		t.Errorf("空文件名应回退为 id+ext，实际 %q", got)
	}
	if got := cleanFilename(strings.Repeat("x", 300)+".jpg", "id1", ".jpg"); got != "id1.jpg" {
		t.Errorf("超长文件名应回退，实际长度 %d", len(got))
	}
}
