package setup

// Order-keeping JSON edits (security audit 2026-11-22, M3): setup changes one key in other tools'
// JSON configs (~/.claude.json, Gemini's settings.json, ...). Going through map[string]any
// reordered every key in the file; this keeps the file as it was except for the one entry.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
)

// omap is a JSON object with its keys in file order.
type omap struct {
	keys []string
	vals map[string]any // *omap, []any, json.Number, string, bool or nil
}

func newOmap() *omap { return &omap{vals: map[string]any{}} }

func (m *omap) get(k string) (any, bool) {
	v, ok := m.vals[k]
	return v, ok
}

// set replaces a key in place, or appends it.
func (m *omap) set(k string, v any) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

func (m *omap) del(k string) {
	if _, ok := m.vals[k]; !ok {
		return
	}
	delete(m.vals, k)
	for i, x := range m.keys {
		if x == k {
			m.keys = append(m.keys[:i:i], m.keys[i+1:]...)
			break
		}
	}
}

func parseOrdered(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, isDelim := t.(json.Delim)
	if !isDelim {
		return t, nil // string, json.Number, bool, nil
	}
	switch d {
	case '{':
		m := newOmap()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, ok := kt.(string)
			if !ok {
				return nil, errors.New("object key isn't a string")
			}
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			m.set(k, v)
		}
		_, err := dec.Token() // the closing brace
		return m, err
	case '[':
		a := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := dec.Token() // the closing bracket
		return a, err
	}
	return nil, errors.New("unexpected delimiter")
}

// marshalOrdered writes v with two-space indentation, objects in their key order.
func marshalOrdered(v any) []byte {
	var b bytes.Buffer
	writeValue(&b, v, "")
	b.WriteByte('\n')
	return b.Bytes()
}

func writeValue(b *bytes.Buffer, v any, ind string) {
	switch x := v.(type) {
	case *omap:
		if len(x.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range x.keys {
			b.WriteString(ind + "  ")
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteString(": ")
			writeValue(b, x.vals[k], ind+"  ")
			if i < len(x.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(ind + "}")
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range x {
			b.WriteString(ind + "  ")
			writeValue(b, e, ind+"  ")
			if i < len(x)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(ind + "]")
	case json.Number:
		b.WriteString(x.String())
	case float64:
		b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	default:
		vb, _ := json.Marshal(x)
		b.Write(vb)
	}
}

// toOrdered turns a Go value (maps, slices, scalars) into the ordered form: an MCP entry's type,
// command, args and env first, any other keys after them in sorted order.
func toOrdered(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := newOmap()
		for _, k := range []string{"type", "command", "args", "env"} {
			if e, ok := x[k]; ok {
				m.set(k, toOrdered(e))
			}
		}
		rest := make([]string, 0, len(x))
		for k := range x {
			if _, done := m.vals[k]; !done {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		for _, k := range rest {
			m.set(k, toOrdered(x[k]))
		}
		return m
	case []string:
		a := make([]any, len(x))
		for i, s := range x {
			a[i] = s
		}
		return a
	case []any:
		a := make([]any, len(x))
		for i, e := range x {
			a[i] = toOrdered(e)
		}
		return a
	}
	return v
}
