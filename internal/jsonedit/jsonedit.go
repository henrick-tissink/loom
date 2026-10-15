// Package jsonedit edits a JSON document in place while keeping the order
// of object members and the bytes of values it does not touch. Claude
// Code, plugins and other tools all rewrite settings.json; an editor that
// reorders keys or re-encodes untouched values would make every loom write
// look like a large change and would lose fields it does not know about.
//
// The model is a tree: objects are ordered member lists, arrays are
// slices, and scalars are kept as the raw bytes they were parsed from.
package jsonedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Kind of node.
type Kind int

const (
	Scalar Kind = iota // string, number, bool, null — kept as raw bytes
	Object
	Array
)

// Node is one JSON value.
type Node struct {
	Kind    Kind
	Raw     json.RawMessage // Scalar only
	Members []Member        // Object only, in document order
	Items   []*Node         // Array only
}

// Member is one key/value pair of an object.
type Member struct {
	Key   string
	Value *Node
}

// Parse decodes a document. Duplicate keys keep their first occurrence's
// position and the last value, which is what encoding/json does.
func Parse(b []byte) (*Node, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	n, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if rest := bytes.TrimSpace(b[dec.InputOffset():]); len(rest) > 0 {
		return nil, errors.New("jsonedit: trailing data after the document")
	}
	return n, nil
}

func parseValue(dec *json.Decoder) (*Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &Node{Kind: Object}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("jsonedit: object key %v is not a string", kt)
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.Set(key, v)
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return n, nil
		case '[':
			n := &Node{Kind: Array}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.Items = append(n.Items, v)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return n, nil
		}
		return nil, fmt.Errorf("jsonedit: unexpected delimiter %v", t)
	case string:
		return &Node{Kind: Scalar, Raw: marshalRaw(t)}, nil
	case json.Number:
		return &Node{Kind: Scalar, Raw: json.RawMessage(t.String())}, nil
	case bool:
		if t {
			return &Node{Kind: Scalar, Raw: json.RawMessage("true")}, nil
		}
		return &Node{Kind: Scalar, Raw: json.RawMessage("false")}, nil
	case nil:
		return &Node{Kind: Scalar, Raw: json.RawMessage("null")}, nil
	}
	return nil, fmt.Errorf("jsonedit: unexpected token %v", tok)
}

// Constructors ---------------------------------------------------------------

func NewObject() *Node { return &Node{Kind: Object} }
func NewArray() *Node  { return &Node{Kind: Array} }

// NewValue encodes any Go value as a node (objects from maps lose order:
// use NewObject and Set when order matters).
func NewValue(v any) *Node {
	b, err := marshalNoEscape(v)
	if err != nil {
		panic("jsonedit: " + err.Error())
	}
	n, err := Parse(b)
	if err != nil {
		panic("jsonedit: " + err.Error())
	}
	return n
}

func String(s string) *Node { return NewValue(s) }
func Int(i int) *Node       { return &Node{Kind: Scalar, Raw: json.RawMessage(strconv.Itoa(i))} }
func Bool(b bool) *Node     { return NewValue(b) }

// Access --------------------------------------------------------------------

// Get returns the member value for key, or nil.
func (n *Node) Get(key string) *Node {
	if n == nil || n.Kind != Object {
		return nil
	}
	for _, m := range n.Members {
		if m.Key == key {
			return m.Value
		}
	}
	return nil
}

// Path walks nested objects; nil when any step is missing.
func (n *Node) Path(keys ...string) *Node {
	cur := n
	for _, k := range keys {
		cur = cur.Get(k)
		if cur == nil {
			return nil
		}
	}
	return cur
}

// Set replaces or appends a member. Appending keeps the new key last, the
// position a human editor would also use.
func (n *Node) Set(key string, v *Node) {
	for i, m := range n.Members {
		if m.Key == key {
			n.Members[i].Value = v
			return
		}
	}
	n.Members = append(n.Members, Member{Key: key, Value: v})
}

// Ensure returns the object member for key, creating an empty object
// when missing.
func (n *Node) Ensure(key string) *Node {
	if v := n.Get(key); v != nil {
		return v
	}
	v := NewObject()
	n.Set(key, v)
	return v
}

// EnsureArray returns the array member for key, creating it when missing.
func (n *Node) EnsureArray(key string) *Node {
	if v := n.Get(key); v != nil {
		return v
	}
	v := NewArray()
	n.Set(key, v)
	return v
}

// Delete removes a member; reports whether it existed.
func (n *Node) Delete(key string) bool {
	if n == nil || n.Kind != Object {
		return false
	}
	for i, m := range n.Members {
		if m.Key == key {
			n.Members = append(n.Members[:i], n.Members[i+1:]...)
			return true
		}
	}
	return false
}

// Append adds an array item.
func (n *Node) Append(v *Node) { n.Items = append(n.Items, v) }

// Filter keeps the array items for which keep returns true; returns how
// many were dropped.
func (n *Node) Filter(keep func(*Node) bool) int {
	if n == nil || n.Kind != Array {
		return 0
	}
	kept := n.Items[:0]
	dropped := 0
	for _, it := range n.Items {
		if keep(it) {
			kept = append(kept, it)
		} else {
			dropped++
		}
	}
	n.Items = kept
	return dropped
}

// Str returns a string scalar's value ("" otherwise).
func (n *Node) Str() string {
	if n == nil || n.Kind != Scalar {
		return ""
	}
	var s string
	if json.Unmarshal(n.Raw, &s) != nil {
		return ""
	}
	return s
}

// IntVal returns a numeric scalar as an int (0 otherwise).
func (n *Node) IntVal() int {
	if n == nil || n.Kind != Scalar {
		return 0
	}
	f, err := strconv.ParseFloat(string(n.Raw), 64)
	if err != nil {
		return 0
	}
	return int(f)
}

// BoolVal returns a boolean scalar's value (false otherwise).
func (n *Node) BoolVal() bool {
	return n != nil && n.Kind == Scalar && string(n.Raw) == "true"
}

// Len is the number of items (array) or members (object).
func (n *Node) Len() int {
	if n == nil {
		return 0
	}
	if n.Kind == Array {
		return len(n.Items)
	}
	return len(n.Members)
}

// Equal compares two nodes structurally.
func (n *Node) Equal(o *Node) bool {
	return bytes.Equal(n.Marshal(""), o.Marshal(""))
}

// Marshal -------------------------------------------------------------------

// Marshal renders the document. indent "" gives compact output; "  " gives
// the two-space layout Claude Code itself writes.
func (n *Node) Marshal(indent string) []byte {
	var b bytes.Buffer
	n.write(&b, indent, 0)
	return b.Bytes()
}

func (n *Node) write(b *bytes.Buffer, indent string, depth int) {
	nl := func(d int) {
		if indent != "" {
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(indent, d))
		}
	}
	switch n.Kind {
	case Scalar:
		b.Write(n.Raw)
	case Object:
		if len(n.Members) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, m := range n.Members {
			if i > 0 {
				b.WriteByte(',')
			}
			nl(depth + 1)
			k := marshalRaw(m.Key)
			b.Write(k)
			b.WriteByte(':')
			if indent != "" {
				b.WriteByte(' ')
			}
			m.Value.write(b, indent, depth+1)
		}
		nl(depth)
		b.WriteByte('}')
	case Array:
		if len(n.Items) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, it := range n.Items {
			if i > 0 {
				b.WriteByte(',')
			}
			nl(depth + 1)
			it.write(b, indent, depth+1)
		}
		nl(depth)
		b.WriteByte(']')
	}
}

// Files ---------------------------------------------------------------------

// ReadFile parses a JSON file. A missing file yields an empty object and
// exists=false, so callers can create it.
func ReadFile(path string) (n *Node, exists bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewObject(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return NewObject(), true, nil
	}
	n, err = Parse(b)
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	return n, true, nil
}

// WriteFile writes the document atomically: through a symlink to the real
// file (a dotfiles symlink stays a symlink), into a temp file in the same
// directory, fsynced, then renamed over the target. The file mode is kept.
func WriteFile(path string, n *Node) error {
	return WriteBytes(path, append(n.Marshal("  "), '\n'))
}

// WriteBytes is WriteFile for already-rendered content.
func WriteBytes(path string, data []byte) error {
	real := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(real); err == nil {
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(real)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(real)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, real); err != nil {
		cleanup()
		return err
	}
	return nil
}

// marshalNoEscape encodes like json.Marshal but leaves <, > and & as they
// are, so a user's hook command survives a rewrite byte for byte.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func marshalRaw(v any) json.RawMessage {
	b, _ := marshalNoEscape(v)
	return b
}
