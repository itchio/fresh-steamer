// Package vdf parses Valve's text KeyValues format, which is what PICS
// returns for app info.
package vdf

import (
	"fmt"
	"strconv"
	"strings"
)

// Node is either a leaf with a Value or a branch with Children. Children
// keep insertion order; keys are compared case-insensitively on lookup.
type Node struct {
	Key      string
	Value    string
	Children []*Node
	leaf     bool
}

func (n *Node) IsLeaf() bool { return n != nil && n.leaf }

// Get finds a direct child by key. Missing keys return nil, and every
// accessor tolerates a nil receiver so lookups chain safely.
func (n *Node) Get(key string) *Node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if strings.EqualFold(c.Key, key) {
			return c
		}
	}
	return nil
}

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

func (n *Node) String() string {
	if n == nil {
		return ""
	}
	return n.Value
}

func (n *Node) Uint64() uint64 {
	if n == nil {
		return 0
	}
	v, _ := strconv.ParseUint(n.Value, 10, 64)
	return v
}

func (n *Node) Uint32() uint32 { return uint32(n.Uint64()) }

func (n *Node) Bool() bool {
	return n != nil && (n.Value == "1" || strings.EqualFold(n.Value, "true"))
}

// Parse reads a document made of one or more top-level keys and returns a
// synthetic root holding them.
func Parse(src []byte) (*Node, error) {
	p := &parser{s: string(src)}
	root := &Node{}
	for {
		p.skipSpace()
		if p.eof() {
			return root, nil
		}
		child, err := p.pair()
		if err != nil {
			return nil, err
		}
		root.Children = append(root.Children, child)
	}
}

type parser struct {
	s   string
	pos int
}

func (p *parser) eof() bool { return p.pos >= len(p.s) }

func (p *parser) skipSpace() {
	for !p.eof() {
		c := p.s[p.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == 0:
			p.pos++
		case c == '/' && p.pos+1 < len(p.s) && p.s[p.pos+1] == '/':
			for !p.eof() && p.s[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *parser) pair() (*Node, error) {
	key, err := p.token()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.eof() {
		return nil, fmt.Errorf("vdf: unexpected end after key %q", key)
	}
	if p.s[p.pos] == '{' {
		p.pos++
		n := &Node{Key: key}
		for {
			p.skipSpace()
			if p.eof() {
				return nil, fmt.Errorf("vdf: unterminated block %q", key)
			}
			if p.s[p.pos] == '}' {
				p.pos++
				return n, nil
			}
			child, err := p.pair()
			if err != nil {
				return nil, err
			}
			n.Children = append(n.Children, child)
		}
	}
	val, err := p.token()
	if err != nil {
		return nil, err
	}
	return &Node{Key: key, Value: val, leaf: true}, nil
}

func (p *parser) token() (string, error) {
	p.skipSpace()
	if p.eof() {
		return "", fmt.Errorf("vdf: unexpected end of input")
	}
	if p.s[p.pos] != '"' {
		start := p.pos
		for !p.eof() {
			c := p.s[p.pos]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '{' || c == '}' {
				break
			}
			p.pos++
		}
		return p.s[start:p.pos], nil
	}
	p.pos++
	var b strings.Builder
	for {
		if p.eof() {
			return "", fmt.Errorf("vdf: unterminated string")
		}
		c := p.s[p.pos]
		p.pos++
		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if p.eof() {
				return "", fmt.Errorf("vdf: unterminated escape")
			}
			e := p.s[p.pos]
			p.pos++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
}
