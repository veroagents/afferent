package mcpconfig

// jsonedit.go edits one member of a JSON object in place. Everything else in
// the file (key order, formatting, other servers, unrelated settings) keeps
// its exact bytes, which matters for ~/.claude.json: Claude Code keeps a lot
// of its own state there and rewrites it often.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type jsonMember struct {
	key                        string
	keyStart, valStart, valEnd int
}

type jsonObject struct {
	open, close int // offsets of '{' and '}'
	members     []jsonMember
}

func (o *jsonObject) find(key string) int {
	for i, m := range o.members {
		if m.key == key {
			return i
		}
	}
	return -1
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// scanObject parses the object that is data[start:end] (surrounding
// whitespace allowed) and returns absolute offsets.
func scanObject(data []byte, start, end int) (*jsonObject, error) {
	seg := data[start:end]
	dec := json.NewDecoder(bytes.NewReader(seg))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	o := &jsonObject{open: start + int(dec.InputOffset()) - 1}
	for dec.More() {
		ks := int(dec.InputOffset())
		for ks < len(seg) && (isSpace(seg[ks]) || seg[ks] == ',') {
			ks++
		}
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		vs := int(dec.InputOffset())
		for vs < len(seg) && (isSpace(seg[vs]) || seg[vs] == ':') {
			vs++
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.members = append(o.members, jsonMember{key: key, keyStart: start + ks, valStart: start + vs, valEnd: start + int(dec.InputOffset())})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	o.close = start + int(dec.InputOffset()) - 1
	if data[o.close] != '}' {
		return nil, errors.New("unexpected end of object")
	}
	return o, nil
}

// lineIndent is the leading whitespace of the line holding offset i.
func lineIndent(data []byte, i int) string {
	ls := bytes.LastIndexByte(data[:i], '\n') + 1
	j := ls
	for j < len(data) && (data[j] == ' ' || data[j] == '\t') {
		j++
	}
	return string(data[ls:j])
}

// indentUnit guesses the file's indentation step from the first member of
// the top-level object ("  " by default).
func indentUnit(data []byte, top *jsonObject) string {
	if len(top.members) > 0 {
		if ind := lineIndent(data, top.members[0].keyStart); ind != "" && strings.Contains(string(data[top.open:top.members[0].keyStart]), "\n") {
			return ind
		}
	}
	return "  "
}

func marshalAt(v any, indent, unit string) ([]byte, error) {
	return json.MarshalIndent(v, indent, unit)
}

// getJSONPath returns the value at path, if present.
func getJSONPath(data []byte, path []string) (json.RawMessage, bool, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false, nil
	}
	start, end := 0, len(data)
	for i, key := range path {
		o, err := scanObject(data, start, end)
		if err != nil {
			if i == 0 {
				return nil, false, err
			}
			return nil, false, nil // a non-object on the way: absent
		}
		k := o.find(key)
		if k < 0 {
			return nil, false, nil
		}
		start, end = o.members[k].valStart, o.members[k].valEnd
	}
	return json.RawMessage(data[start:end]), true, nil
}

// setJSONPath sets the value at path (creating objects on the way) and
// returns the new document.
func setJSONPath(data []byte, path []string, val any) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		v := val
		for i := len(path) - 1; i >= 0; i-- {
			v = map[string]any{path[i]: v}
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	}
	top, err := scanObject(data, 0, len(data))
	if err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	unit := indentUnit(data, top)
	start, end := 0, len(data)
	o := top
	for depth, key := range path {
		if depth > 0 {
			if o, err = scanObject(data, start, end); err != nil {
				return nil, fmt.Errorf("%q is not a JSON object", strings.Join(path[:depth], "."))
			}
		}
		k := o.find(key)
		if k < 0 {
			// Insert key: {rest of path...: val} as the object's last member.
			v := val
			for i := len(path) - 1; i > depth; i-- {
				v = map[string]any{path[i]: v}
			}
			objIndent := lineIndent(data, o.open)
			childIndent := objIndent + unit
			vb, err := marshalAt(v, childIndent, unit)
			if err != nil {
				return nil, err
			}
			kb, _ := json.Marshal(key)
			member := "\n" + childIndent + string(kb) + ": " + string(vb)
			var out bytes.Buffer
			if n := len(o.members); n > 0 {
				at := o.members[n-1].valEnd
				out.Write(data[:at])
				out.WriteString("," + member)
				out.Write(data[at:])
			} else {
				out.Write(data[:o.open+1])
				out.WriteString(member + "\n" + objIndent)
				out.Write(data[o.close:])
			}
			return checked(out.Bytes())
		}
		start, end = o.members[k].valStart, o.members[k].valEnd
		if depth == len(path)-1 {
			vb, err := marshalAt(val, lineIndent(data, o.members[k].keyStart), unit)
			if err != nil {
				return nil, err
			}
			var out bytes.Buffer
			out.Write(data[:start])
			out.Write(vb)
			out.Write(data[end:])
			return checked(out.Bytes())
		}
	}
	return nil, errors.New("empty path")
}

// deleteJSONPath removes the member at path. It reports false when absent.
func deleteJSONPath(data []byte, path []string) ([]byte, bool, error) {
	if len(bytes.TrimSpace(data)) == 0 || len(path) == 0 {
		return data, false, nil
	}
	start, end := 0, len(data)
	var o *jsonObject
	var k int
	for depth, key := range path {
		var err error
		if o, err = scanObject(data, start, end); err != nil {
			if depth == 0 {
				return nil, false, fmt.Errorf("not a JSON object: %w", err)
			}
			return data, false, nil
		}
		if k = o.find(key); k < 0 {
			return data, false, nil
		}
		start, end = o.members[k].valStart, o.members[k].valEnd
	}
	var from, to int
	switch {
	case len(o.members) == 1:
		from, to = o.open+1, o.close
	case k > 0:
		from, to = o.members[k-1].valEnd, o.members[k].valEnd
	default:
		from, to = o.members[0].keyStart, o.members[1].keyStart
	}
	out := append(append([]byte(nil), data[:from]...), data[to:]...)
	b, err := checked(out)
	return b, err == nil, err
}

func checked(b []byte) ([]byte, error) {
	if !json.Valid(b) {
		return nil, errors.New("internal error: the edit would produce invalid JSON; nothing was written")
	}
	return b, nil
}

// unifiedDiff is a small line diff for --dry-run: the changed middle of the
// file with three lines of context (edits here are always one region).
func unifiedDiff(path string, before, after []byte) string {
	if bytes.Equal(before, after) {
		return ""
	}
	a := splitLines(before)
	b := splitLines(after)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	const ctx = 3
	lo := max(p-ctx, 0)
	aHi, bHi := min(len(a)-s+ctx, len(a)), min(len(b)-s+ctx, len(b))
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s (afferent)\n", path, path)
	fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", lo+1, aHi-lo, lo+1, bHi-lo)
	for i := lo; i < p; i++ {
		out.WriteString(" " + a[i] + "\n")
	}
	for i := p; i < len(a)-s; i++ {
		out.WriteString("-" + a[i] + "\n")
	}
	for i := p; i < len(b)-s; i++ {
		out.WriteString("+" + b[i] + "\n")
	}
	for i := len(a) - s; i < aHi; i++ {
		out.WriteString(" " + a[i] + "\n")
	}
	return out.String()
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}
