package supervisor

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"unicode"
)

var marker = []byte("[REDACTED]")

func secretLiterals(values map[string]string) ([][]byte, error) {
	literals := map[string]bool{}
	for _, value := range values {
		literals[value] = true
		var document any
		if err := json.Unmarshal([]byte(value), &document); err != nil {
			// The decoder's own depth limit must not bypass sensitive-field inspection.
			if strings.Contains(err.Error(), "exceeded max depth") {
				return nil, invalid("structured secret exceeds inspection limits")
			}
			continue
		}
		nodes := 0
		var visit func(any, bool, int) error
		visit = func(current any, sensitive bool, depth int) error {
			nodes++
			if depth > 32 || nodes > 4096 || len(literals) > 256 {
				return invalid("structured secret exceeds inspection limits")
			}
			switch v := current.(type) {
			case map[string]any:
				for key, child := range v {
					key = strings.Map(func(r rune) rune {
						if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
							return r
						}
						return -1
					}, strings.ToLower(key))
					private := sensitive || slices.Contains([]string{"passwd", "authorization", "bearer", "sessionkey", "accesskeysecret", "cookie"}, key)
					for _, suffix := range []string{"token", "secret", "password", "privatekey", "apikey"} {
						private = private || strings.HasSuffix(key, suffix)
					}
					if err := visit(child, private, depth+1); err != nil {
						return err
					}
				}
			case []any:
				for _, child := range v {
					if err := visit(child, sensitive, depth+1); err != nil {
						return err
					}
				}
			case string:
				if sensitive && v != "" {
					literals[v] = true
				}
			}
			return nil
		}
		if err := visit(document, false, 0); err != nil {
			return nil, err
		}
	}
	if len(literals) > 256 {
		return nil, invalid("structured secret exceeds inspection limits")
	}
	result := make([][]byte, 0, len(literals))
	for value := range literals {
		if value == "" || bytes.Contains(marker, []byte(value)) {
			return nil, invalid("secret conflicts with the redaction marker")
		}
		result = append(result, []byte(value))
	}
	slices.SortFunc(result, func(a, b []byte) int { return cmp.Compare(len(b), len(a)) })
	return result, nil
}

type redactor struct {
	values  [][]byte
	pending []byte
}

func (r *redactor) feed(chunk []byte, final bool) []byte {
	r.pending = append(r.pending, chunk...)
	var out []byte
	index, literal := 0, 0
	for index < len(r.pending) {
		remaining := r.pending[index:]
		if !final && slices.ContainsFunc(r.values, func(v []byte) bool { return len(v) > len(remaining) && bytes.HasPrefix(v, remaining) }) {
			break
		}
		match := slices.IndexFunc(r.values, func(v []byte) bool { return bytes.HasPrefix(remaining, v) })
		if match >= 0 {
			out = append(out, r.pending[literal:index]...)
			out = append(out, marker...)
			index += len(r.values[match])
			literal = index
		} else {
			index++
		}
	}
	out = append(out, r.pending[literal:index]...)
	r.pending = bytes.Clone(r.pending[index:])
	return out
}

func validEnv(name, value string) bool {
	if name == "" || strings.HasPrefix(name, "MAKEZ_") || strings.ContainsRune(value, 0) {
		return false
	}
	for i, r := range name {
		if r != '_' && (r >= 128 || !unicode.IsLetter(r)) && (i <= 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}
