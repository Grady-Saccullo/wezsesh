// Package canonicaljson implements the §4 canonical-JSON encoder for the
// Go side of the IPC wire protocol. It produces byte-identical output to
// the Lua encoder (plugin/wezsesh/canonical_json.lua) for every shape in
// the §17.1 golden corpus. CI gate: LC_ALL=C go test ./internal/canonicaljson/...
//
// Encoding rules (§4.1) are applied literally:
//   - Object keys sorted by unsigned UTF-8 byte order, recursively.
//   - No whitespace.
//   - Integers only ([-2^63, 2^63-1]); floats are an error (ErrFloat).
//   - Strings are UTF-8 validated (ErrInvalidUTF8). Escape \\ and \";
//     escape U+0000–U+001F, U+007F, U+0080–U+009F, U+2028, U+2029 as
//     \u00XX (lowercase hex). Forward slash is NEVER escaped.
//     Short-form \b \f \n \r \t are FORBIDDEN.
//   - Booleans: true/false (lowercase). Null: null.
//   - Empty object MUST emit {}; empty array MUST emit [] — disambiguation
//     on the Go side is by type (map vs slice). Use Null for explicit null.
//
// There is no Unmarshal: canonicality is a property of outbound bytes
// only. Reply parsing uses encoding/json (§8.2).
package canonicaljson

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"unicode/utf8"
)

var (
	// ErrFloat is returned for any float subtype encountered during
	// Marshal. §4.1 rule 3 forbids floats; the wire is integer-only.
	ErrFloat = errors.New("canonicaljson: float not allowed")

	// ErrInvalidUTF8 is returned when a string contains invalid UTF-8.
	ErrInvalidUTF8 = errors.New("canonicaljson: invalid UTF-8")

	// ErrUnsupported is returned for types the encoder cannot represent
	// (channels, functions, complex, custom map keys, etc.).
	ErrUnsupported = errors.New("canonicaljson: unsupported type")

	// ErrIntOverflow is returned when a uint exceeds int64 max.
	ErrIntOverflow = errors.New("canonicaljson: integer out of int64 range")
)

// nullType is the explicit-null sentinel. Use Null (the exported
// singleton) to emit a JSON null. nil values are rejected to keep
// "missing" and "explicitly null" disambiguated at the source.
type nullType struct{}

// Null is the sentinel emitted as JSON null. The Lua side mirrors this
// with canonical_json.NULL (§4.1 rule 6).
var Null = nullType{}

// Marshal encodes v per §4.1. Returns ErrFloat for any float subtype,
// ErrInvalidUTF8 for invalid UTF-8 strings, ErrUnsupported for
// unsupported types. The bytes returned are stable: re-encoding the
// same input produces the same bytes, byte-for-byte.
func Marshal(v any) ([]byte, error) {
	var buf []byte
	buf, err := appendValue(buf, v)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func appendValue(buf []byte, v any) ([]byte, error) {
	if v == nil {
		// Untyped nil is ambiguous (was it a missing field, an empty
		// map, an empty slice?). The Lua side rejects nil values too;
		// callers must use Null explicitly for JSON null.
		return nil, fmt.Errorf("%w: untyped nil; use canonicaljson.Null for explicit null", ErrUnsupported)
	}

	switch x := v.(type) {
	case nullType:
		return append(buf, 'n', 'u', 'l', 'l'), nil
	case bool:
		if x {
			return append(buf, 't', 'r', 'u', 'e'), nil
		}
		return append(buf, 'f', 'a', 'l', 's', 'e'), nil
	case string:
		return appendString(buf, x)
	case int:
		return strconv.AppendInt(buf, int64(x), 10), nil
	case int8:
		return strconv.AppendInt(buf, int64(x), 10), nil
	case int16:
		return strconv.AppendInt(buf, int64(x), 10), nil
	case int32:
		return strconv.AppendInt(buf, int64(x), 10), nil
	case int64:
		return strconv.AppendInt(buf, x, 10), nil
	case uint:
		if uint64(x) > math.MaxInt64 {
			return nil, ErrIntOverflow
		}
		return strconv.AppendUint(buf, uint64(x), 10), nil
	case uint8:
		return strconv.AppendUint(buf, uint64(x), 10), nil
	case uint16:
		return strconv.AppendUint(buf, uint64(x), 10), nil
	case uint32:
		return strconv.AppendUint(buf, uint64(x), 10), nil
	case uint64:
		if x > math.MaxInt64 {
			return nil, ErrIntOverflow
		}
		return strconv.AppendUint(buf, x, 10), nil
	case float32, float64:
		return nil, ErrFloat
	case map[string]any:
		return appendObject(buf, x)
	case []any:
		return appendArray(buf, x)
	}

	// Reflective fallback for typed maps/slices/named scalar types.
	return appendReflect(buf, reflect.ValueOf(v))
}

func appendReflect(buf []byte, rv reflect.Value) ([]byte, error) {
	if !rv.IsValid() {
		return nil, fmt.Errorf("%w: invalid reflect.Value", ErrUnsupported)
	}

	switch rv.Kind() {
	case reflect.Bool:
		if rv.Bool() {
			return append(buf, 't', 'r', 'u', 'e'), nil
		}
		return append(buf, 'f', 'a', 'l', 's', 'e'), nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(buf, rv.Int(), 10), nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := rv.Uint()
		if u > math.MaxInt64 {
			return nil, ErrIntOverflow
		}
		return strconv.AppendUint(buf, u, 10), nil

	case reflect.Float32, reflect.Float64:
		return nil, ErrFloat

	case reflect.String:
		return appendString(buf, rv.String())

	case reflect.Slice, reflect.Array:
		// Empty/nil typed slice → []. The slice type itself disambiguates.
		n := rv.Len()
		buf = append(buf, '[')
		for i := 0; i < n; i++ {
			if i > 0 {
				buf = append(buf, ',')
			}
			var err error
			buf, err = appendValue(buf, rv.Index(i).Interface())
			if err != nil {
				return nil, err
			}
		}
		buf = append(buf, ']')
		return buf, nil

	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("%w: map key must be string, got %s", ErrUnsupported, rv.Type().Key().Kind())
		}
		keys := make([]string, 0, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			keys = append(keys, iter.Key().String())
		}
		// §4.1.1: unsigned UTF-8 byte order. Go's < on string compares
		// bytes (Go strings are immutable byte sequences), so
		// sort.Strings yields the required order under any locale.
		sort.Strings(keys)
		buf = append(buf, '{')
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			var err error
			buf, err = appendString(buf, k)
			if err != nil {
				return nil, err
			}
			buf = append(buf, ':')
			buf, err = appendValue(buf, rv.MapIndex(reflect.ValueOf(k)).Interface())
			if err != nil {
				return nil, err
			}
		}
		buf = append(buf, '}')
		return buf, nil

	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			return nil, fmt.Errorf("%w: nil %s; use canonicaljson.Null for explicit null", ErrUnsupported, rv.Kind())
		}
		return appendValue(buf, rv.Elem().Interface())

	case reflect.Struct:
		// Treat the Null sentinel specially.
		if rv.Type() == reflect.TypeOf(nullType{}) {
			return append(buf, 'n', 'u', 'l', 'l'), nil
		}
		return nil, fmt.Errorf("%w: struct types are unsupported (use map[string]any)", ErrUnsupported)
	}

	return nil, fmt.Errorf("%w: %s", ErrUnsupported, rv.Kind())
}

func appendObject(buf []byte, m map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf = append(buf, '{')
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		var err error
		buf, err = appendString(buf, k)
		if err != nil {
			return nil, err
		}
		buf = append(buf, ':')
		buf, err = appendValue(buf, m[k])
		if err != nil {
			return nil, err
		}
	}
	buf = append(buf, '}')
	return buf, nil
}

func appendArray(buf []byte, a []any) ([]byte, error) {
	buf = append(buf, '[')
	for i, v := range a {
		if i > 0 {
			buf = append(buf, ',')
		}
		var err error
		buf, err = appendValue(buf, v)
		if err != nil {
			return nil, err
		}
	}
	buf = append(buf, ']')
	return buf, nil
}

// appendString emits a JSON string per §4.1.4.
//
// Escape policy:
//   - U+005C (\) → \\
//   - U+0022 (") → \"
//   - U+0000–U+001F, U+007F, U+0080–U+009F → \u00xx (lowercase hex)
//   - U+2028, U+2029 →   /   (lowercase hex)
//   - All other code points (≥ U+0020 except those above) emitted raw.
//
// Forward slash (U+002F) is NEVER escaped. Short-form \b \f \n \r \t
// are FORBIDDEN — the \u00xx form is canonical.
func appendString(buf []byte, s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, ErrInvalidUTF8
	}
	const hex = "0123456789abcdef"
	buf = append(buf, '"')
	i := 0
	for i < len(s) {
		c := s[i]
		// Fast path for plain ASCII >= 0x20 that needs no escape and is
		// not 0x22 (") or 0x5c (\) or 0x7f (DEL).
		if c >= 0x20 && c < 0x7f && c != '"' && c != '\\' {
			buf = append(buf, c)
			i++
			continue
		}
		// Decode the rune to handle multibyte escapes (LS/PS) and to
		// advance past valid multibyte sequences emitted raw.
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\\':
			buf = append(buf, '\\', '\\')
		case r == '"':
			buf = append(buf, '\\', '"')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// Two-byte hex form: \u00XX. Forbidden short-forms map here.
			buf = append(buf, '\\', 'u', '0', '0', hex[(r>>4)&0xf], hex[r&0xf])
		case r == 0x2028 || r == 0x2029:
			buf = append(buf, '\\', 'u',
				hex[(r>>12)&0xf], hex[(r>>8)&0xf],
				hex[(r>>4)&0xf], hex[r&0xf])
		default:
			// Raw UTF-8 bytes — including code points in the BMP and
			// supplementary planes.
			buf = append(buf, s[i:i+size]...)
		}
		i += size
	}
	buf = append(buf, '"')
	return buf, nil
}
