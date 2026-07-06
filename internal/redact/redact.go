// Package redact masks secrets and local absolute paths before any data leaves
// the machine (DESIGN §8). It is a faithful Go port of omp-langfuse's
// src/redaction.ts: the same regexes, the same limits, the same replacement
// order.
//
// The session log is Zero's un-redacted private transcript (DESIGN §8.2), so
// redaction is entirely this package's responsibility before upload.
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"regexp"
	"sort"
)

// Redacted is the placeholder substituted for matched secrets.
const Redacted = "[REDACTED_SECRET]"

// RedactOptions bounds how deep and wide redaction recurses. Mirrors
// redaction.ts RedactOptions/DEFAULT_OPTIONS.
type RedactOptions struct {
	MaxDepth        int
	MaxArrayItems   int
	MaxObjectKeys   int
	MaxStringLength int
}

// DefaultRedactOptions matches omp defaults (redaction.ts:12-17).
var DefaultRedactOptions = RedactOptions{
	MaxDepth:        6,
	MaxArrayItems:   50,
	MaxObjectKeys:   80,
	MaxStringLength: 12000,
}

// Patterns ported verbatim from redaction.ts:19-28. RE2 (Go regexp) supports
// all of them; none use lookaround/backreferences.
var (
	secretAssignmentRE = regexp.MustCompile(
		`(?i)\b([A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASS|API[_-]?KEY|PRIVATE[_-]?KEY|AUTH|COOKIE)[A-Z0-9_]*)\s*=\s*([^\s"` + "\"`" + `]+)`)
	privateKeyRE = regexp.MustCompile(
		`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)
	bearerRE = regexp.MustCompile(
		`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{12,}`)
	knownTokenRE = regexp.MustCompile(
		`\b(?:sk-(?:lf|ant|proj|live|test)[A-Za-z0-9_-]*|pk-lf-[A-Za-z0-9_-]+|gh[pousr]_[A-Za-z0-9_]{20,}|npm_[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16})\b`)
	absolutePathRE = regexp.MustCompile(
		"(?:/Users/[^/\\s]+|/home/[^/\\s]+|/private/tmp|/tmp|[A-Za-z]:\\\\Users\\\\[^\\\\\\s]+)(?:[^\\s\"'`]*)")
	sensitiveFieldRE = regexp.MustCompile(
		`^(?i)(authorization|cookie|setcookie|xapikey|apikey|token|accesstoken|refreshtoken|secret|secretkey|password|passwd|privatekey)$`)
	// envSuffixRE captures a trailing /.env* or \.env* suffix to preserve after
	// hashing the preceding path (redaction.ts:47-49).
	envSuffixRE = regexp.MustCompile(`([/\\]\.env(?:\.[A-Za-z0-9_-]+)?)$`)
)

// HashPath returns a stable, non-reversible token for an absolute path
// (redaction.ts:30-32). The hash is sha256, truncated to 12 hex chars.
func HashPath(p string) string {
	sum := sha256.Sum256([]byte(p))
	return "[PATH_HASH:" + hex.EncodeToString(sum[:])[:12] + "]"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "... [truncated]"
}

// RedactString truncates then masks secrets/paths in omp's fixed order:
// privateKey → bearer → knownToken → secretAssignment (key kept, value masked)
// → absolutePath (hashed, env-suffix preserved). Mirrors redactString
// (redaction.ts:38-50).
func RedactString(s string, o RedactOptions) string {
	s = truncate(s, o.MaxStringLength)
	s = privateKeyRE.ReplaceAllString(s, Redacted)
	s = bearerRE.ReplaceAllString(s, Redacted)
	s = knownTokenRE.ReplaceAllString(s, Redacted)
	// Keep the key name, mask the value: <key>=<Redacted>.
	s = secretAssignmentRE.ReplaceAllString(s, "${1}="+Redacted)
	s = absolutePathRE.ReplaceAllStringFunc(s, func(path string) string {
		suffix := ""
		if m := envSuffixRE.FindStringSubmatchIndex(path); m != nil {
			// m[2],m[3] are the group's bounds within path.
			suffix = path[m[2]:m[3]]
			path = path[:m[2]]
		}
		return HashPath(path) + suffix
	})
	return s
}

// RedactValue recursively redacts an arbitrary value: scalars pass through;
// strings → RedactString; funcs/chans → [...]; reference types are tracked for
// cycles; arrays/maps are truncated to the option limits. Sensitive object
// keys (authorization, cookie, apikey, …) have their *value* replaced wholesale.
// Mirrors visit/redactValue (redaction.ts:52-115).
//
// Map keys are sorted alphabetically (Go maps are unordered); this makes the
// redacted payload deterministic, which omp's insertion-order traversal does
// not guarantee but which is desirable for reproducible traces.
func RedactValue(v any, o RedactOptions) any {
	return visit(v, o, o.MaxDepth, map[uintptr]bool{})
}

func visit(v any, o RedactOptions, depth int, seen map[uintptr]bool) any {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case bool:
		return x
	case int:
		return x
	case int64:
		return x
	case float64:
		return x
	case float32:
		return x
	case string:
		return RedactString(x, o)
	}

	// Anything non-reference that's not a recognized scalar becomes a string.
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil
	}
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint()
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	case reflect.String:
		return RedactString(rv.String(), o)
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return "[" + rv.Kind().String() + "]"
	}

	// depth exhausted before descending into a container.
	if depth <= 0 {
		return "[max depth " + itoa(o.MaxDepth) + " reached]"
	}

	// Cycle check (reference types only — value types can't cycle).
	if ptr, ok := refKey(rv); ok {
		if seen[ptr] {
			return "[circular]"
		}
		seen[ptr] = true
		defer delete(seen, ptr)
	}

	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		n := rv.Len()
		out := make([]any, 0, imin(n, o.MaxArrayItems)+1)
		limit := imin(n, o.MaxArrayItems)
		for i := 0; i < limit; i++ {
			out = append(out, visit(rv.Index(i).Interface(), o, depth-1, seen))
		}
		if n > o.MaxArrayItems {
			out = append(out, "["+itoa(n-o.MaxArrayItems)+" truncated items]")
		}
		return out
	case reflect.Map:
		keys := rv.MapKeys()
		// Deterministic ordering for reproducible payloads.
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		out := make(map[string]any, imin(len(keys), o.MaxObjectKeys)+1)
		limit := imin(len(keys), o.MaxObjectKeys)
		for i := 0; i < limit; i++ {
			k := keys[i].String()
			nk := normalizeKey(k)
			if sensitiveFieldRE.MatchString(nk) {
				out[k] = Redacted
			} else {
				out[k] = visit(rv.MapIndex(keys[i]).Interface(), o, depth-1, seen)
			}
		}
		if len(keys) > o.MaxObjectKeys {
			out["__truncatedKeys"] = len(keys) - o.MaxObjectKeys
		}
		return out
	case reflect.Ptr, reflect.Interface:
		// Unwrap one level and re-visit (keeps depth/seen consistent).
		if rv.IsZero() {
			return nil
		}
		return visit(rv.Elem().Interface(), o, depth, seen)
	case reflect.Struct:
		t := rv.Type()
		out := make(map[string]any, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			k := f.Name
			nk := normalizeKey(k)
			if sensitiveFieldRE.MatchString(nk) {
				out[k] = Redacted
			} else {
				out[k] = visit(rv.Field(i).Interface(), o, depth-1, seen)
			}
		}
		return out
	}

	// Fallback: stringify.
	return RedactString(rv.String(), o)
}

func refKey(rv reflect.Value) (uintptr, bool) {
	switch rv.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return rv.Pointer(), true
	}
	return 0, false
}

func normalizeKey(k string) string {
	var b []byte
	for _, r := range k {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			b = append(b, byte(r))
		}
	}
	return string(b)
}

func imin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
