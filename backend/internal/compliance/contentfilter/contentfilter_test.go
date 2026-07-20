package contentfilter

import (
	"testing"
)

func TestDo_SensitiveHit_Blocks(t *testing.T) {
	f := New()
	res := f.Do("来参与诈骗活动吧")
	if res.Passed {
		t.Fatal("sensitive hit should block")
	}
	if res.Text != "" {
		t.Fatalf("blocked text should be empty, got %q", res.Text)
	}
	if len(res.HitWords) == 0 || res.HitWords[0] != "诈骗" {
		t.Fatalf("expect hit 诈骗, got %v", res.HitWords)
	}
}

func TestDo_NoHit_PassesUnchanged(t *testing.T) {
	f := New()
	res := f.Do("今天红富士特价5元一斤")
	if !res.Passed {
		t.Fatal("clean text should pass")
	}
	if res.Text != "今天红富士特价5元一斤" {
		t.Fatalf("text should be unchanged, got %q", res.Text)
	}
	if len(res.HitWords) != 0 || len(res.Replaced) != 0 {
		t.Fatalf("no hits expected, got hits=%v replaced=%v", res.HitWords, res.Replaced)
	}
}

// Absolutism replacement is whole-word → replacement string (so replacement may
// be "*" for masking or "很" for a real substitute word).
func TestDo_Absolutism_Replaced(t *testing.T) {
	f := New()
	res := f.Do("全县最低价的水果")
	if !res.Passed {
		t.Fatal("absolutism should not block")
	}
	if res.Text != "全县*的水果" {
		t.Fatalf("expect 最低价->* (whole-word), got %q", res.Text)
	}
	found := false
	for _, w := range res.Replaced {
		if w == "最低价" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expect 最低价 in Replaced, got %v", res.Replaced)
	}
}

func TestDo_AbsolutismMultiple_ReplacedDedup(t *testing.T) {
	f := New()
	// 第一 appears twice; should be replaced twice but listed once
	res := f.Do("第一选择, 绝对第一")
	if !res.Passed {
		t.Fatal("absolutism should not block")
	}
	count := 0
	for _, w := range res.Replaced {
		if w == "第一" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("第一 should be deduped to 1 in Replaced, got %d", count)
	}
}

func TestDo_SensitiveTakesPrecedenceOverAbsolutism(t *testing.T) {
	f := New()
	// text has both sensitive (诈骗) and absolutism (第一) → sensitive wins, blocks
	res := f.Do("第一名的诈骗犯")
	if res.Passed {
		t.Fatal("sensitive should take precedence and block")
	}
	if len(res.Replaced) != 0 {
		t.Fatalf("absolutism should not run when blocked, got %v", res.Replaced)
	}
}

func TestDo_ChineseRuneMatching(t *testing.T) {
	f := New(WithSensitiveWords([]string{"临期", "过期食品"}), WithAbsolutismWords(nil))
	// DFA must match multibyte words at rune granularity
	res := f.Do("这批是过期食品别卖了")
	if res.Passed {
		t.Fatal("should block on 过期食品")
	}
	if res.HitWords[0] != "过期食品" {
		t.Fatalf("expect 过期食品, got %v", res.HitWords)
	}
}

func TestDo_CustomWordsOverride(t *testing.T) {
	f := New(
		WithSensitiveWords([]string{"竞品A", "竞品B"}),
		WithAbsolutismWords([]string{"超便宜"}),
		WithReplacement("X"),
	)
	// custom sensitive
	res := f.Do("比竞品A好")
	if res.Passed {
		t.Fatal("custom sensitive should block")
	}
	// custom absolutism
	res = f.Do("超便宜的果子")
	if !res.Passed {
		t.Fatal("custom absolutism should pass")
	}
	if res.Text != "X的果子" {
		t.Fatalf("expect X replacement, got %q", res.Text)
	}
}

func TestDFA_LongestMatch(t *testing.T) {
	d := newDFA()
	d.add("赌博")
	d.add("赌博网站")
	// at position of 赌博网站, should match the longer 赌博网站, not just 赌博
	hits := d.matchAll("访问赌博网站赚钱")
	if len(hits) != 1 || hits[0] != "赌博网站" {
		t.Fatalf("expect longest match 赌博网站, got %v", hits)
	}
}
