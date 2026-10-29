package agent

import (
	"bytes"
	"encoding/json"

	"github.com/azeem-yousaf/grove/internal/usage"
)

// usageFields reads a provider's usage object, or nil if it isn't one
// (missing, null, or not an object).
func usageFields(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// count reads a token count from a usage object. present is false when the
// field is absent or null; ok is false when it is there but not a whole,
// non-negative number.
func count(m map[string]json.RawMessage, key string) (n int64, present, ok bool) {
	raw, there := m[key]
	if !there || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false, true
	}
	if json.Unmarshal(raw, &n) != nil || n < 0 {
		return 0, true, false
	}
	return n, true, true
}

// anthropicUsage is one message's usage as Anthropic reports it, built up
// from message_start and message_delta. Its input_tokens leaves out the
// cache, so Input adds both cache figures back.
type anthropicUsage struct {
	input, cacheWrite, cacheRead, output int64
	hasInput, hasOutput, hasCache        bool
}

// merge takes the fields raw has; fields it leaves out keep their value. It
// returns false, changing nothing, if raw isn't a usage object or holds a
// field that isn't a count.
func (a *anthropicUsage) merge(raw json.RawMessage) bool {
	return a.mergeFields(raw, "input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens")
}

// mergeFields is merge with the field names given: modelUsage uses
// inputTokens and so on, with the same meanings.
func (a *anthropicUsage) mergeFields(raw json.RawMessage, input, output, cacheWrite, cacheRead string) bool {
	m := usageFields(raw)
	if m == nil {
		return false
	}
	next := *a
	for _, f := range []struct {
		key  string
		dst  *int64
		seen *bool
	}{
		{input, &next.input, &next.hasInput},
		{output, &next.output, &next.hasOutput},
		{cacheWrite, &next.cacheWrite, &next.hasCache},
		{cacheRead, &next.cacheRead, &next.hasCache},
	} {
		n, present, ok := count(m, f.key)
		if !ok {
			return false
		}
		if present {
			*f.dst, *f.seen = n, true
		}
	}
	*a = next
	return true
}

// tokens is the usage in grove's terms; ok is false until both the input
// and the output side are known.
func (a anthropicUsage) tokens() (t usage.Tokens, ok bool) {
	if !a.hasInput || !a.hasOutput {
		return usage.Tokens{}, false
	}
	return usage.Tokens{
		Input:      a.input + a.cacheWrite + a.cacheRead,
		Output:     a.output,
		CacheRead:  a.cacheRead,
		CacheWrite: a.cacheWrite,
		HasCache:   a.hasCache,
	}, true
}

// openAIUsage reads the usage of a Codex turn. OpenAI's input_tokens
// already includes cached_input_tokens, and output_tokens already includes
// reasoning_output_tokens (checked with a real `codex exec --json` turn:
// 23 output tokens of which 15 reasoning, for a four-character answer), so
// neither is added again. cache_write_input_tokens is taken to be part of
// input_tokens like the cached ones; it was 0 when checked, so that is
// assumed.
func openAIUsage(raw json.RawMessage) (usage.Tokens, bool) {
	m := usageFields(raw)
	if m == nil {
		return usage.Tokens{}, false
	}
	in, hasIn, ok1 := count(m, "input_tokens")
	out, hasOut, ok2 := count(m, "output_tokens")
	cached, hasCached, ok3 := count(m, "cached_input_tokens")
	written, hasWritten, ok4 := count(m, "cache_write_input_tokens")
	if !ok1 || !ok2 || !ok3 || !ok4 || !hasIn || !hasOut || cached+written > in {
		return usage.Tokens{}, false
	}
	return usage.Tokens{Input: in, Output: out, CacheRead: cached, CacheWrite: written, HasCache: hasCached || hasWritten}, true
}
