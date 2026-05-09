package web

import (
	"reflect"
	"strings"
)

// toInt64 coerces common numeric inputs (int kinds, uint kinds, floats,
// json.Number-as-string) into an int64. Returns false for everything else.
func toInt64(v any) (int64, bool) {
	if v == nil {
		return 0, false
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return 0, false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := rv.Uint()
		return int64(u), true
	case reflect.Float32, reflect.Float64:
		return int64(rv.Float()), true
	default:
		return 0, false
	}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// fieldByPath walks a chain of field names through a struct (and any
// pointer/interface indirection), returning the leaf value or nil.
func fieldByPath(v any, path ...string) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	for _, name := range path {
		for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
			if rv.IsNil() {
				return nil
			}
			rv = rv.Elem()
		}
		if rv.Kind() != reflect.Struct {
			return nil
		}
		rv = rv.FieldByName(name)
		if !rv.IsValid() {
			return nil
		}
	}
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() || !rv.CanInterface() {
		return nil
	}
	return rv.Interface()
}

// fieldSlice fetches a slice-typed field from v and returns it as []any.
// Returns nil if the field is absent or not a slice.
func fieldSlice(v any, name string) []any {
	raw := fieldByPath(v, name)
	if raw == nil {
		return nil
	}
	rv := reflect.ValueOf(raw)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil
	}
	out := make([]any, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		el := rv.Index(i)
		if el.CanInterface() {
			out[i] = el.Interface()
		}
	}
	return out
}
