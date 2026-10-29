package usage

// Tokens is what one agent turn, or a sum of turns, used as its provider reported it.
// Input counts every prompt token, cached ones included, so Total never counts the cache twice.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read,omitempty"`  // part of Input read from the provider's cache
	CacheWrite int64 `json:"cache_write,omitempty"` // part of Input written to the cache
	HasCache   bool  `json:"has_cache,omitempty"`   // the provider reported cache figures
}

// Total is every token the turn used: input (cached included) plus output.
func (t Tokens) Total() int64 { return t.Input + t.Output }

// Add sums two figures; HasCache is true if either has it.
func (t Tokens) Add(o Tokens) Tokens {
	return Tokens{
		Input:      t.Input + o.Input,
		Output:     t.Output + o.Output,
		CacheRead:  t.CacheRead + o.CacheRead,
		CacheWrite: t.CacheWrite + o.CacheWrite,
		HasCache:   t.HasCache || o.HasCache,
	}
}

// IsZero reports whether nothing was counted.
func (t Tokens) IsZero() bool { return t == Tokens{} }

// Status says how far a turn's figure can be trusted.
type Status uint8

const (
	Unknown Status = iota // the provider reported nothing usable
	Partial               // the turn was cut short: only what was reported before it stopped, a lower bound
	Exact                 // the provider's own figure for the whole turn
)

func (s Status) String() string {
	switch s {
	case Partial:
		return "partial"
	case Exact:
		return "exact"
	}
	return "unknown"
}

// MarshalText writes the status as "unknown", "partial" or "exact".
func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText reads what MarshalText wrote; any other text is Unknown.
func (s *Status) UnmarshalText(b []byte) error {
	switch string(b) {
	case "partial":
		*s = Partial
	case "exact":
		*s = Exact
	default:
		*s = Unknown
	}
	return nil
}
