package strictjson

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

var unmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// checkExact walks the document alongside the target type and refuses any
// object key that is not exactly a declared json tag name of the struct it
// decodes into (encoding/json would also accept "Admin" or the long-s fold
// "write_<long s>copes"). Types with their own UnmarshalJSON check themselves
// when they decode. Map keys, RawMessage and interface values are free.
func checkExact(val any, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(unmarshalerType) || reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil
	}
	switch v := val.(type) {
	case map[string]any:
		switch t.Kind() {
		case reflect.Struct:
			fields := map[string]reflect.Type{}
			collect(t, fields)
			for k, sub := range v {
				ft, ok := fields[k]
				if !ok {
					return fmt.Errorf("unknown field %q (field names are exact)", k)
				}
				if err := checkExact(sub, ft); err != nil {
					return err
				}
			}
		case reflect.Map:
			for _, sub := range v {
				if err := checkExact(sub, t.Elem()); err != nil {
					return err
				}
			}
		}
	case []any:
		if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			for _, sub := range v {
				if err := checkExact(sub, t.Elem()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func collect(t reflect.Type, out map[string]reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collect(ft, out)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
}
