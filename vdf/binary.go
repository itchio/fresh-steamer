package vdf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Binary KeyValues type tags.
const (
	binNone    = 0
	binString  = 1
	binInt32   = 2
	binFloat32 = 3
	binPointer = 4
	binWString = 5
	binColor   = 6
	binUint64  = 7
	binEnd     = 8
	binInt64   = 10
	binEndAlt  = 11
)

// ParseBinary reads one binary KeyValues object, the encoding PICS uses for
// package info. Numeric values become their decimal string form so the
// accessors on Node work the same as for text documents.
func ParseBinary(src []byte) (*Node, error) {
	r := &binReader{b: src}
	root := &Node{}
	for {
		if r.eof() {
			return root, nil
		}
		t := r.b[r.pos]
		if t == binEnd || t == binEndAlt {
			return root, nil
		}
		n, err := r.node()
		if err != nil {
			return nil, err
		}
		root.Children = append(root.Children, n)
	}
}

type binReader struct {
	b   []byte
	pos int
}

func (r *binReader) eof() bool { return r.pos >= len(r.b) }

func (r *binReader) cstring() (string, error) {
	start := r.pos
	for r.pos < len(r.b) {
		if r.b[r.pos] == 0 {
			s := string(r.b[start:r.pos])
			r.pos++
			return s, nil
		}
		r.pos++
	}
	return "", errors.New("vdf: unterminated string")
}

func (r *binReader) take(n int) ([]byte, error) {
	if r.pos+n > len(r.b) {
		return nil, errors.New("vdf: truncated binary value")
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v, nil
}

func (r *binReader) node() (*Node, error) {
	tb, err := r.take(1)
	if err != nil {
		return nil, err
	}
	t := tb[0]
	key, err := r.cstring()
	if err != nil {
		return nil, err
	}
	n := &Node{Key: key, leaf: true}
	switch t {
	case binNone:
		n.leaf = false
		for {
			if r.eof() {
				return nil, fmt.Errorf("vdf: unterminated object %q", key)
			}
			if r.b[r.pos] == binEnd || r.b[r.pos] == binEndAlt {
				r.pos++
				return n, nil
			}
			child, err := r.node()
			if err != nil {
				return nil, err
			}
			n.Children = append(n.Children, child)
		}
	case binString, binWString:
		n.Value, err = r.cstring()
	case binInt32, binColor, binPointer:
		v, e := r.take(4)
		err = e
		if e == nil {
			n.Value = strconv.FormatInt(int64(int32(binary.LittleEndian.Uint32(v))), 10)
		}
	case binFloat32:
		v, e := r.take(4)
		err = e
		if e == nil {
			n.Value = strconv.FormatFloat(float64(math.Float32frombits(binary.LittleEndian.Uint32(v))), 'g', -1, 32)
		}
	case binUint64:
		v, e := r.take(8)
		err = e
		if e == nil {
			n.Value = strconv.FormatUint(binary.LittleEndian.Uint64(v), 10)
		}
	case binInt64:
		v, e := r.take(8)
		err = e
		if e == nil {
			n.Value = strconv.FormatInt(int64(binary.LittleEndian.Uint64(v)), 10)
		}
	default:
		return nil, fmt.Errorf("vdf: unknown binary type %d for key %q", t, key)
	}
	return n, err
}
