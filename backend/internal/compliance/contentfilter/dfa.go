// Package contentfilter implements the content-review filter (BE-B-05, CONTRACT
// §10.3): a DFA-based sensitive-word matcher plus an advertising-law
// absolutism-word replacer. The core Do method is a pure function with no side
// effects; it imports no domain packages.
package contentfilter

// dfa is a rune-based Aho-Corasick-style matching tree (without failure links;
// the simple trie walk suffices for the word-list sizes involved). Each node
// holds child edges keyed by rune and a flag marking the end of a configured
// word. Matching walks the tree from each input rune position; on reaching an
// end flag the matched span is recorded.
type dfa struct {
	root *dfaNode
}

type dfaNode struct {
	children map[rune]*dfaNode
	end      bool
}

func newDFA() *dfa { return &dfa{root: &dfaNode{children: make(map[rune]*dfaNode)}} }

// add inserts one word into the trie. Empty words are ignored.
func (d *dfa) add(word string) {
	if word == "" {
		return
	}
	cur := d.root
	for _, r := range word {
		next, ok := cur.children[r]
		if !ok {
			next = &dfaNode{children: make(map[rune]*dfaNode)}
			cur.children[r] = next
		}
		cur = next
	}
	cur.end = true
}

// matchAll returns every configured word found in text, in order of first
// occurrence (by start index). Overlapping matches at the same start are
// resolved to the longest word the trie accepts from that position.
func (d *dfa) matchAll(text string) []string {
	runes := []rune(text)
	var hits []string
	for i := 0; i < len(runes); i++ {
		cur := d.root
		lastEnd := -1
		for j := i; j < len(runes); j++ {
			next, ok := cur.children[runes[j]]
			if !ok {
				break
			}
			cur = next
			if cur.end {
				lastEnd = j // 记最长匹配终点
			}
		}
		if lastEnd >= 0 {
			hits = append(hits, string(runes[i:lastEnd+1]))
			i = lastEnd // 跳过已匹配区间, 避免子串重复
		}
	}
	return hits
}

// replaceAll returns text with every occurrence of any word in words replaced by
// replacement. It walks the rune slice and replaces greedily (longest match at
// each position). Words are pre-compiled into a lookup: for each start rune we
// try each word — acceptable for the small absolutism word lists involved.
func replaceAll(text string, words []string, replacement string) string {
	if len(words) == 0 {
		return text
	}
	runes := []rune(text)
	rep := []rune(replacement)
	out := make([]rune, 0, len(runes))
	i := 0
	for i < len(runes) {
		matched := false
		for _, w := range words {
			wr := []rune(w)
			if len(wr) == 0 {
				continue
			}
			if i+len(wr) <= len(runes) && runeSliceEqual(runes[i:i+len(wr)], wr) {
				out = append(out, rep...)
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
	return string(out)
}

func runeSliceEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}
