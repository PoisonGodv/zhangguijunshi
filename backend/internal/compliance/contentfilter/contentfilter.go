package contentfilter

// Filter is the content-review filter (BE-B-05). It combines a DFA-based
// sensitive-word matcher (hard block — content is rejected) with an
// advertising-law absolutism-word replacer (soft — words are replaced but the
// content is still sendable). The Do method is a pure function.
//
// Default word lists are a minimal usable set; callers override them via the
// With* options (the main thread will wire a config-backed list in production).

// Result is the outcome of one Do call.
type Result struct {
	Text     string   // 过滤/替换后的文本; 命中敏感词时为空
	Passed   bool     // true=可发送, false=命中敏感词被拦截
	HitWords []string // 命中的敏感词(DFA)
	Replaced []string // 被替换的绝对化词(广告法)
}

// Filter holds the compiled DFA, the absolutism word list, and the replacement
// rune used for absolutism words.
type Filter struct {
	dfa         *dfa
	absolutism  []string
	replacement string
}

// Option customises a Filter.
type Option func(*Filter)

// WithSensitiveWords overrides the default sensitive-word list.
func WithSensitiveWords(words []string) Option {
	return func(f *Filter) {
		d := newDFA()
		for _, w := range words {
			d.add(w)
		}
		f.dfa = d
	}
}

// WithAbsolutismWords overrides the default absolutism-word list.
func WithAbsolutismWords(words []string) Option {
	return func(f *Filter) { f.absolutism = append([]string(nil), words...) }
}

// WithReplacement overrides the absolutism replacement string (default "*").
func WithReplacement(r string) Option {
	return func(f *Filter) {
		if r != "" {
			f.replacement = r
		}
	}
}

// defaultSensitiveWords is the minimal built-in sensitive-word set. Production
// deployments override this with a full word list via WithSensitiveWords.
var defaultSensitiveWords = []string{
	"诈骗", "赌博", "色情", "违禁品", "毒品", "枪支",
}

// defaultAbsolutismWords is the advertising-law absolutism set (广告法绝对化
// 用语): words like 最/第一/国家级 that the law restricts in commercial copy.
var defaultAbsolutismWords = []string{
	"国家级", "世界级", "最高级", "第一", "顶级", "极品", "唯一", "首个",
	"首选", "绝无仅有", "极致", "万能", "百分百", "最佳", "最优", "最低价",
}

// New constructs a Filter with the default word lists compiled in.
func New(opts ...Option) *Filter {
	d := newDFA()
	for _, w := range defaultSensitiveWords {
		d.add(w)
	}
	f := &Filter{
		dfa:         d,
		absolutism:  append([]string(nil), defaultAbsolutismWords...),
		replacement: "*",
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Do reviews text: DFA sensitive-word hits take precedence (Passed=false,
// Text=""); if no sensitive word hits, absolutism words are replaced with the
// replacement string (Passed=true, Text=cleaned). The two checks are
// independent and never both apply to the same call.
func (f *Filter) Do(text string) Result {
	// 1. DFA 敏感词命中 → 硬拦截
	hits := f.dfa.matchAll(text)
	if len(hits) > 0 {
		return Result{
			Text:     "",
			Passed:   false,
			HitWords: hits,
		}
	}

	// 2. 绝对化词替换(软, 可发)
	cleaned, replaced := f.replaceAbsolutism(text)
	return Result{
		Text:     cleaned,
		Passed:   true,
		Replaced: replaced,
	}
}

// replaceAbsolutism replaces every absolutism word in text with the replacement
// string and returns the cleaned text plus the set of words that were replaced
// (de-duplicated, in first-occurrence order).
func (f *Filter) replaceAbsolutism(text string) (string, []string) {
	if len(f.absolutism) == 0 {
		return text, nil
	}
	runes := []rune(text)
	rep := []rune(f.replacement)
	out := make([]rune, 0, len(runes))
	seen := make(map[string]bool)
	var replaced []string
	i := 0
	for i < len(runes) {
		matched := false
		for _, w := range f.absolutism {
			wr := []rune(w)
			if len(wr) == 0 {
				continue
			}
			if i+len(wr) <= len(runes) && runeSliceEqual(runes[i:i+len(wr)], wr) {
				out = append(out, rep...)
				if !seen[w] {
					seen[w] = true
					replaced = append(replaced, w)
				}
				i += len(wr)
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, runes[i])
			i++
		}
	}
	return string(out), replaced
}
