package config

import (
	"fmt"
	"reflect"
	"strings"
)

// interpolate expands ${VAR}, ${VAR:-default} and $${ (literal "${") in every
// string value of v (a pointer to a struct), after YAML decoding. Errors name
// the variable and the field, never a value.
func interpolate(v any, env func(string) (string, bool)) []error {
	var errs []error
	walk(reflect.ValueOf(v).Elem(), "", env, &errs)
	return errs
}

func walk(v reflect.Value, path string, env func(string) (string, bool), errs *[]error) {
	switch v.Kind() {
	case reflect.String:
		out, err := expand(v.String(), env)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("%s: %w", path, err))
			return
		}
		v.SetString(out)
	case reflect.Pointer:
		if !v.IsNil() {
			walk(v.Elem(), path, env, errs)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
			if name == "" {
				name = strings.ToLower(sf.Name)
			}
			walk(v.Field(i), join(path, name), env, errs)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			walk(v.Index(i), fmt.Sprintf("%s[%d]", path, i), env, errs)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			cp := reflect.New(v.Type().Elem()).Elem()
			cp.Set(v.MapIndex(k))
			walk(cp, join(path, fmt.Sprint(k.Interface())), env, errs)
			v.SetMapIndex(k, cp)
		}
	}
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func isVarStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isVarChar(c byte) bool { return isVarStart(c) || (c >= '0' && c <= '9') }

func expand(s string, env func(string) (string, bool)) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "$${") {
			b.WriteString("${")
			i += 3
			continue
		}
		if !strings.HasPrefix(s[i:], "${") {
			b.WriteByte('$')
			i++
			continue
		}
		j := i + 2
		if j >= len(s) || !isVarStart(s[j]) {
			return "", fmt.Errorf("malformed ${...} reference (use $${ for a literal)")
		}
		k := j
		for k < len(s) && isVarChar(s[k]) {
			k++
		}
		name := s[j:k]
		def, hasDef := "", false
		if strings.HasPrefix(s[k:], ":-") {
			hasDef = true
			end := strings.IndexByte(s[k:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated ${%s:-...} reference", name)
			}
			def = s[k+2 : k+end]
			k += end
		}
		if k >= len(s) || s[k] != '}' {
			return "", fmt.Errorf("malformed ${%s...} reference (use $${ for a literal)", name)
		}
		val, ok := env(name)
		switch {
		case ok && val != "":
			b.WriteString(val)
		case hasDef:
			b.WriteString(def)
		case ok:
			// defined but empty, no default: expands to empty
		default:
			return "", fmt.Errorf("environment variable %s is not defined", name)
		}
		i = k + 1
	}
	return b.String(), nil
}
