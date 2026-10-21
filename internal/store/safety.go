package store

import (
	"bytes"
	"regexp"
)

// injectionRe matches instruction-like text aimed at AI systems (docs/04 §8,
// "injection canaries"). It's a cheap tripwire, not a defence: the real
// defence is that components reading untrusted text can't act. Flagged
// documents are still stored; their observations are tagged and down-weighted.
var injectionRe = regexp.MustCompile(`(?i)(ignore (all |any )?(the )?(previous|prior|above) (instructions|prompts?)|disregard (all |the )?(previous|prior|above)|you are (now )?(an? )?(ai|assistant|language model|llm)\b|system prompt|<\s*/?\s*(system|assistant|instructions?)\s*>|\bas an ai (language )?model\b|new instructions:|developer mode|jailbreak)`)

// LooksLikeInjection reports whether content contains prompt-injection markers.
func LooksLikeInjection(content []byte) bool {
	if len(content) > 2<<20 {
		content = content[:2<<20]
	}
	return injectionRe.Match(bytes.ToValidUTF8(content, nil))
}
