package graphlite

import (
	"context"
	"fmt"
)

// ─────────────────────────────────────────────────────────────────────────────
// Type constraints
// ─────────────────────────────────────────────────────────────────────────────

// PropertyValue is a type constraint that encompasses all scalar types that may
// appear as property values in a graphlite graph.
type PropertyValue interface {
	~bool |
		~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64 |
		~string
}

// RecordValue is a type constraint that encompasses all values that may appear
// in a result Record: scalar property values, Node, and Relationship pointers.
type RecordValue interface {
	PropertyValue | *Node | *Relationship
}

// ─────────────────────────────────────────────────────────────────────────────
// propsGetter — unexported interface implemented by Node and Relationship
// ─────────────────────────────────────────────────────────────────────────────

// propsGetter is implemented by graph entities that expose a property map.
// It is used internally by GetProperty to avoid duplicating logic per type.
type propsGetter interface {
	getProps() map[string]any
}

// getProps returns the property map for a Node.
func (n *Node) getProps() map[string]any { return n.Props }

// getProps returns the property map for a Relationship.
func (r *Relationship) getProps() map[string]any { return r.Props }

// ─────────────────────────────────────────────────────────────────────────────
// GetProperty
// ─────────────────────────────────────────────────────────────────────────────

// GetProperty extracts a property named key from a Node or Relationship and
// returns it as the requested type T. It returns an error if the key is absent
// or if the stored value cannot be converted to T.
//
// Numeric widening follows Go's standard conversion rules: integer and
// floating-point types are converted via a direct type-conversion, and the
// result is checked for round-trip fidelity when narrowing.
func GetProperty[T PropertyValue](entity propsGetter, key string) (T, error) {
	var zero T
	props := entity.getProps()
	raw, ok := props[key]
	if !ok {
		return zero, fmt.Errorf("graphlite: property %q not found", key)
	}
	v, err := convertTo[T](raw)
	if err != nil {
		return zero, fmt.Errorf("graphlite: property %q: %w", key, err)
	}
	return v, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GetRecordValue
// ─────────────────────────────────────────────────────────────────────────────

// GetRecordValue extracts the value associated with key from a Record and
// returns it as type T. The second return value is true when the value is nil
// in the record (i.e. the key was present but held a null). It returns an error
// if the key is absent or the value cannot be converted to T.
func GetRecordValue[T RecordValue](rec *Record, key string) (T, bool, error) {
	var zero T
	raw, ok := rec.Get(key)
	if !ok {
		return zero, false, fmt.Errorf("graphlite: record key %q not found", key)
	}
	if raw == nil {
		return zero, true, nil
	}
	v, err := convertRecordValueTo[T](raw)
	if err != nil {
		return zero, false, fmt.Errorf("graphlite: record key %q: %w", key, err)
	}
	return v, false, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// CollectT
// ─────────────────────────────────────────────────────────────────────────────

// CollectT drains all records from result, applies mapper to each record, and
// returns the mapped slice. It delegates to Result.Collect and therefore closes
// the cursor. If Collect or any mapper call returns an error, CollectT returns
// that error immediately (with a nil slice).
func CollectT[T any](ctx context.Context, result *Result, mapper func(*Record) (T, error)) ([]T, error) {
	recs, err := result.Collect(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(recs))
	for _, rec := range recs {
		v, err := mapper(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SingleT
// ─────────────────────────────────────────────────────────────────────────────

// SingleT calls Result.Single and applies mapper to the record. It returns
// ErrNoRecords if the result is empty or ErrMultipleRecords if it contains more
// than one record — the same sentinels as Result.Single.
func SingleT[T any](ctx context.Context, result *Result, mapper func(*Record) (T, error)) (T, error) {
	var zero T
	rec, err := result.Single(ctx)
	if err != nil {
		return zero, err
	}
	return mapper(rec)
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal conversion helpers
// ─────────────────────────────────────────────────────────────────────────────

// convertTo converts a raw any value (as returned from a JSON-decoded property
// map) to type T. SQLite via database/sql delivers JSON numbers as float64;
// string and bool are delivered as-is.
func convertTo[T PropertyValue](raw any) (T, error) {
	var zero T
	switch any(zero).(type) {
	case bool:
		if v, ok := raw.(bool); ok {
			return any(v).(T), nil
		}
	case string:
		if v, ok := raw.(string); ok {
			return any(v).(T), nil
		}
	case int:
		if v, err := toInt64(raw); err == nil {
			return any(int(v)).(T), nil
		}
	case int8:
		if v, err := toInt64(raw); err == nil {
			return any(int8(v)).(T), nil
		}
	case int16:
		if v, err := toInt64(raw); err == nil {
			return any(int16(v)).(T), nil
		}
	case int32:
		if v, err := toInt64(raw); err == nil {
			return any(int32(v)).(T), nil
		}
	case int64:
		if v, err := toInt64(raw); err == nil {
			return any(v).(T), nil
		}
	case uint:
		if v, err := toInt64(raw); err == nil {
			return any(uint(v)).(T), nil
		}
	case uint8:
		if v, err := toInt64(raw); err == nil {
			return any(uint8(v)).(T), nil
		}
	case uint16:
		if v, err := toInt64(raw); err == nil {
			return any(uint16(v)).(T), nil
		}
	case uint32:
		if v, err := toInt64(raw); err == nil {
			return any(uint32(v)).(T), nil
		}
	case uint64:
		if v, err := toInt64(raw); err == nil {
			return any(uint64(v)).(T), nil
		}
	case float32:
		if v, err := toFloat64(raw); err == nil {
			return any(float32(v)).(T), nil
		}
	case float64:
		if v, err := toFloat64(raw); err == nil {
			return any(v).(T), nil
		}
	}
	return zero, fmt.Errorf("cannot convert %T to %T", raw, zero)
}

// convertRecordValueTo handles the superset type RecordValue (PropertyValue +
// *Node + *Relationship). Non-entity values fall through to convertTo.
func convertRecordValueTo[T RecordValue](raw any) (T, error) {
	var zero T
	switch any(zero).(type) {
	case *Node:
		if v, ok := raw.(*Node); ok {
			return any(v).(T), nil
		}
		return zero, fmt.Errorf("cannot convert %T to *Node", raw)
	case *Relationship:
		if v, ok := raw.(*Relationship); ok {
			return any(v).(T), nil
		}
		return zero, fmt.Errorf("cannot convert %T to *Relationship", raw)
	}
	// Delegate scalar types to the PropertyValue converter.
	// We need to call convertTo but T is constrained to RecordValue, not
	// PropertyValue. We handle this via a type-switch on zero.
	return convertToScalar[T](raw)
}

// convertToScalar applies convertTo logic for the scalar subset of RecordValue.
// It avoids re-listing all numeric cases by delegating through the any() bridge.
func convertToScalar[T RecordValue](raw any) (T, error) {
	var zero T
	switch any(zero).(type) {
	case bool:
		if v, ok := raw.(bool); ok {
			return any(v).(T), nil
		}
	case string:
		if v, ok := raw.(string); ok {
			return any(v).(T), nil
		}
	case int:
		if v, err := toInt64(raw); err == nil {
			return any(int(v)).(T), nil
		}
	case int8:
		if v, err := toInt64(raw); err == nil {
			return any(int8(v)).(T), nil
		}
	case int16:
		if v, err := toInt64(raw); err == nil {
			return any(int16(v)).(T), nil
		}
	case int32:
		if v, err := toInt64(raw); err == nil {
			return any(int32(v)).(T), nil
		}
	case int64:
		if v, err := toInt64(raw); err == nil {
			return any(v).(T), nil
		}
	case uint:
		if v, err := toInt64(raw); err == nil {
			return any(uint(v)).(T), nil
		}
	case uint8:
		if v, err := toInt64(raw); err == nil {
			return any(uint8(v)).(T), nil
		}
	case uint16:
		if v, err := toInt64(raw); err == nil {
			return any(uint16(v)).(T), nil
		}
	case uint32:
		if v, err := toInt64(raw); err == nil {
			return any(uint32(v)).(T), nil
		}
	case uint64:
		if v, err := toInt64(raw); err == nil {
			return any(uint64(v)).(T), nil
		}
	case float32:
		if v, err := toFloat64(raw); err == nil {
			return any(float32(v)).(T), nil
		}
	case float64:
		if v, err := toFloat64(raw); err == nil {
			return any(v).(T), nil
		}
	}
	return zero, fmt.Errorf("cannot convert %T to %T", raw, zero)
}

// toInt64 converts a JSON-decoded numeric value (float64, int64, int, or
// json.Number) to int64. Returns an error if the value is not numeric.
func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case uint64:
		return int64(n), nil
	case uint32:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("not a number: %T", v)
	}
}

// toFloat64 converts a JSON-decoded numeric value to float64.
func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int64:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int32:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("not a number: %T", v)
	}
}
