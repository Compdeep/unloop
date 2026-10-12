package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Decode reads one JSON value the way a browser's JSON.parse does, keeping
// numbers as their text until Canonical formats them: objects as
// map[string]any, arrays as []any, numbers as json.Number. Anything after the
// value is refused.
func Decode(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

// Canonical is RFC 8785 of a value Decode returned: keys ordered by UTF-16
// code unit, no whitespace, and strings and numbers written exactly as
// ECMAScript's JSON.stringify writes them, which is what the RFC is defined
// by and what chain/client/spec.ts's `canonical` calls.
//
// A number is a double, as it is in the browser: an integer beyond 2^53 is
// rounded to one first, the same way on both sides. A string Go decoded from
// a lone surrogate escape is not the browser's string, so a message carrying
// one fails its signature here rather than verifying differently.
func Canonical(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := write(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func write(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeString(b, x)
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return fmt.Errorf("RFC 8785 has no encoding for %s", x)
		}
		s, err := number(f)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := write(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			if err := write(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("RFC 8785 has no encoding for %T", v)
	}
	return nil
}

// utf16Less orders two keys as ECMAScript compares strings: by UTF-16 code
// unit. It differs from byte order wherever a character above U+FFFF meets
// one between U+E000 and U+FFFF.
func utf16Less(a, b string) bool {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

// writeString escapes what JSON.stringify escapes and nothing else: the
// quote, the backslash, and control characters — the five with a short form
// by it, the rest as \u00xx. Unlike encoding/json, <, >, & and U+2028/U+2029
// are written as themselves.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// number is ECMAScript's Number::toString for a finite double: the shortest
// digits that read back as the same double, placed by the spec's rules for
// when to write an exponent. Negative zero is "0".
func number(f float64) (string, error) {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return "", errors.New("RFC 8785 has no encoding for a non-finite number")
	}
	if f == 0 {
		return "0", nil
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// d.ddde±x: the shortest digits, and where the point goes.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exponent, _ := strings.Cut(e, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	x, err := strconv.Atoi(exponent)
	if err != nil {
		return "", err
	}
	k, n := len(digits), x+1 // k digits, the point after the n-th
	var s string
	switch {
	case k <= n && n <= 21:
		s = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		s = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		s = "0." + strings.Repeat("0", -n) + digits
	default:
		s = digits[:1]
		if k > 1 {
			s += "." + digits[1:]
		}
		if n-1 < 0 {
			s += "e-" + strconv.Itoa(1-n)
		} else {
			s += "e+" + strconv.Itoa(n-1)
		}
	}
	return sign + s, nil
}
