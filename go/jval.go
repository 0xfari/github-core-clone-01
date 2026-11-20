// jval.go — ordered JSON values with JavaScript-compatible serialization.
//
// The forge's responses must be byte-identical to the TypeScript oracle's
// JSON.stringify output (the smoke diff compares raw bodies). Go's
// encoding/json sorts map keys alphabetically and HTML-escapes <>&, so it
// cannot reproduce Node's output. This type keeps object keys in insertion
// order and escapes strings exactly the way JSON.stringify does: only ",
// \, the C0 controls (\n \r \t \b \f, the rest as \u00XX), and U+2028/U+2029
// are escaped; every other rune (including <, >, &, and all non-ASCII) is
// emitted as raw UTF-8.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"
)

type JKind int

const (
	JNull JKind = iota
	JBool
	JInt
	JFloat
	JString
	JArray
	JObject
)

type JPair struct {
	K string
	V *JVal
}

type JVal struct {
	Kind JKind
	B    bool
	I    int64
	F    float64
	S    string
	A    []*JVal
	O    []JPair
}

func JNullVal() *JVal              { return &JVal{Kind: JNull} }
func JBoolVal(b bool) *JVal        { return &JVal{Kind: JBool, B: b} }
func JIntVal(n int64) *JVal        { return &JVal{Kind: JInt, I: n} }
func JFloatVal(f float64) *JVal    { return &JVal{Kind: JFloat, F: f} }
func JStrVal(s string) *JVal       { return &JVal{Kind: JString, S: s} }
func JArrVal(items ...*JVal) *JVal { return &JVal{Kind: JArray, A: items} }
func JObjVal(pairs ...JPair) *JVal { return &JVal{Kind: JObject, O: pairs} }

// Obj builds an object from alternating key/value arguments:
// Obj("id", JIntVal(1), "name", JStrVal("x")). Keys must be strings.
func Obj(kv ...any) *JVal {
	o := &JVal{Kind: JObject}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		v, _ := kv[i+1].(*JVal)
		if v == nil {
			v = JNullVal()
		}
		o.O = append(o.O, JPair{K: k, V: v})
	}
	return o
}

func Arr(items ...*JVal) *JVal { return &JVal{Kind: JArray, A: items} }

// Get returns the value for key, or nil when absent / not an object.
func (v *JVal) Get(key string) *JVal {
	if v == nil || v.Kind != JObject {
		return nil
	}
	for _, p := range v.O {
		if p.K == key {
			return p.V
		}
	}
	return nil
}

// Str returns the string value and whether this is a JSON string.
func (v *JVal) Str() (string, bool) {
	if v != nil && v.Kind == JString {
		return v.S, true
	}
	return "", false
}

// Bool returns the boolean value and whether this is a JSON boolean.
func (v *JVal) Bool() (bool, bool) {
	if v != nil && v.Kind == JBool {
		return v.B, true
	}
	return false, false
}

// Int returns the integer value and whether this is a JSON number that is
// integral. A float that is mathematically integral also qualifies.
func (v *JVal) Int() (int64, bool) {
	if v == nil {
		return 0, false
	}
	if v.Kind == JInt {
		return v.I, true
	}
	if v.Kind == JFloat && v.F == float64(int64(v.F)) {
		return int64(v.F), true
	}
	return 0, false
}

// IsArray reports whether this is a JSON array.
func (v *JVal) IsArray() bool { return v != nil && v.Kind == JArray }

// IsObject reports whether this is a JSON object.
func (v *JVal) IsObject() bool { return v != nil && v.Kind == JObject }

// Marshal renders the value as JSON bytes, keys in insertion order.
func (v *JVal) Marshal() []byte {
	var buf bytes.Buffer
	v.writeTo(&buf)
	return buf.Bytes()
}

func (v *JVal) writeTo(buf *bytes.Buffer) {
	switch v.Kind {
	case JNull:
		buf.WriteString("null")
	case JBool:
		if v.B {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case JInt:
		buf.WriteString(strconv.FormatInt(v.I, 10))
	case JFloat:
		buf.WriteString(jsFloat(v.F))
	case JString:
		writeJSONString(buf, v.S)
	case JArray:
		buf.WriteByte('[')
		for i, item := range v.A {
			if i > 0 {
				buf.WriteByte(',')
			}
			item.writeTo(buf)
		}
		buf.WriteByte(']')
	case JObject:
		buf.WriteByte('{')
		for i, p := range v.O {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, p.K)
			buf.WriteByte(':')
			p.V.writeTo(buf)
		}
		buf.WriteByte('}')
	}
}

// writeJSONString escapes a string the way JSON.stringify does.
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 || r == 0x2028 || r == 0x2029 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// jsFloat formats a float the way JavaScript's Number.prototype.toString
// does for the values this forge can emit (integers and short decimals).
// Response bodies never carry floats; this exists for completeness.
func jsFloat(f float64) string {
	if f == float64(int64(f)) && f < 1e21 && f > -1e21 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ParseJSON parses a JSON document into an ordered JVal, preserving object
// key order and distinguishing integers from floats. It returns an error
// for malformed input, mirroring JSON.parse.
func ParseJSON(data []byte) (*JVal, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (*JVal, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := &JVal{Kind: JObject}
			for dec.More() {
				ktok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := ktok.(string)
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				obj.O = append(obj.O, JPair{K: key, V: val})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := &JVal{Kind: JArray}
			for dec.More() {
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				arr.A = append(arr.A, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %q", string(t))
	case string:
		return JStrVal(t), nil
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return JIntVal(n), nil
		}
		f, err := t.Float64()
		if err != nil {
			return nil, err
		}
		return JFloatVal(f), nil
	case bool:
		return JBoolVal(t), nil
	case nil:
		return JNullVal(), nil
	}
	return nil, fmt.Errorf("unexpected token %T", tok)
}

// utf8Len is kept for parity checks in tests; valid UTF-8 is assumed.
var _ = utf8.RuneLen
