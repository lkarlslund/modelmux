package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// object is a JSON object that keeps its key order and the exact bytes of
// every value, so editing a few keys of settings.json leaves the rest of the
// file as Claude Code wrote it.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object { return &object{vals: map[string]json.RawMessage{}} }

func parseObject(b []byte) (*object, error) {
	o := newObject()
	if len(bytes.TrimSpace(b)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("want a JSON object")
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(k, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after the JSON object")
	}
	return o, nil
}

// child returns the object under k, or a new empty one.
func (o *object) child(k string) (*object, error) {
	raw, ok := o.vals[k]
	if !ok || string(raw) == "null" {
		return newObject(), nil
	}
	c, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", k, err)
	}
	return c, nil
}

func (o *object) has(k string) bool { _, ok := o.vals[k]; return ok }

func (o *object) str(k string) (string, bool) {
	var s string
	raw, ok := o.vals[k]
	if !ok || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// set stores v (a json.RawMessage, *object, or anything json can encode),
// keeping k's position if it exists.
func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = raw(v)
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, kk := range o.keys {
		if kk == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *object) len() int { return len(o.keys) }

func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(raw(k))
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// format renders o the way Claude Code writes settings.json.
func (o *object) format() []byte {
	m, _ := o.MarshalJSON()
	var c, out bytes.Buffer
	_ = json.Compact(&c, m)
	_ = json.Indent(&out, c.Bytes(), "", "  ")
	out.WriteByte('\n')
	return out.Bytes()
}

// raw encodes v without HTML escaping (json.Marshal would turn < into <).
func raw(v any) json.RawMessage {
	switch v := v.(type) {
	case json.RawMessage:
		return v
	case *object:
		b, _ := v.MarshalJSON()
		return b
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}
