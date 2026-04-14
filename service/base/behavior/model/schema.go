package model

// FieldSchema describes the ordered layout of numeric fields stored
// in a behavior sorted set member. Used to encode/decode events with
// zero-allocation pipe-separated format: "event_id|val0|val1|...".
type FieldSchema struct {
	// Fields is the ordered list of numeric field names referenced by
	// aggregation rules for a given behavior.
	Fields []string
	// Index maps field name → position in Fields for O(1) lookup.
	Index map[string]int
}

// Position returns the slot index of the given field name, or -1 if absent.
func (s *FieldSchema) Position(name string) int {
	if s == nil {
		return -1
	}
	if p, ok := s.Index[name]; ok {
		return p
	}
	return -1
}

// Len returns the number of fields in the schema.
func (s *FieldSchema) Len() int {
	if s == nil {
		return 0
	}
	return len(s.Fields)
}
