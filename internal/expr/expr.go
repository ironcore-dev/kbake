// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package expr implements the small expression language used inside a
// Kernelfile. Two related but distinct mechanisms are supported, mirroring
// GitHub Actions syntax:
//
//   - Variable substitution: bare references like $arch inside string fields
//     are replaced with the variable's value.
//
//   - Expression evaluation: ${{ ... }} blocks contain a boolean expression
//     (e.g. arch == 'arm') that is evaluated against the variable scope. This
//     is used by the `if` attribute of resource references.
//
// The expression grammar supports string/number/boolean literals,
// identifiers, the comparison operators == != < <= > >= and the logical
// operators && || ! with the usual precedence and parentheses.
package expr

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Scope maps variable names to their string values. kbake always populates
// at least "arch".
type Scope map[string]string

// Lookup returns the value of a variable, reporting whether it exists.
func (s Scope) Lookup(name string) (string, bool) {
	v, ok := s[name]
	return v, ok
}

// Subst replaces bare $identifier references in s with their values from the
// scope. Unknown references are left untouched. $$ is an escaped dollar sign.
func (s Scope) Subst(in string) string {
	var b strings.Builder
	i := 0
	for i < len(in) {
		c := in[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		// Escaped dollar.
		if i+1 < len(in) && in[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		// ${{ ... }} expression inside a string evaluates to its value.
		if strings.HasPrefix(in[i:], "${{") {
			if end := strings.Index(in[i:], "}}"); end > 0 {
				inner := strings.TrimSpace(in[i+3 : i+end])
				val, err := EvalString(inner, s)
				if err == nil {
					b.WriteString(val)
				}
				i += end + 2
				continue
			}
		}
		// Bare $identifier.
		j := i + 1
		for j < len(in) && isIdentRune(in[j], j == i+1) {
			j++
		}
		if j == i+1 {
			// Lone $ not followed by an identifier; emit verbatim.
			b.WriteByte('$')
			i++
			continue
		}
		name := in[i+1 : j]
		if v, ok := s.Lookup(name); ok {
			b.WriteString(v)
		} else {
			b.WriteString(in[i:j])
		}
		i = j
	}
	return b.String()
}

func isIdentRune(r byte, first bool) bool {
	if r == '_' {
		return true
	}
	if r >= '0' && r <= '9' {
		return !first
	}
	if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
		return true
	}
	return false
}

// EvalBool evaluates a boolean expression. It accepts either a bare
// expression or one wrapped in ${{ }}.
func EvalBool(in string, s Scope) (bool, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return true, nil
	}
	if strings.HasPrefix(in, "${{") && strings.HasSuffix(in, "}}") {
		in = strings.TrimSpace(in[3 : len(in)-2])
	}
	v, err := eval(in, s)
	if err != nil {
		return false, err
	}
	return v.truthy(), nil
}

// EvalString evaluates an expression and returns its value rendered as a
// string (booleans render as "true"/"false").
func EvalString(in string, s Scope) (string, error) {
	v, err := eval(strings.TrimSpace(in), s)
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

// --- value model ---

type valueKind int

const (
	kBool valueKind = iota
	kStr
	kNum
	kNull
)

type value struct {
	kind valueKind
	b    bool
	s    string
	n    float64
}

func (v value) String() string {
	switch v.kind {
	case kBool:
		if v.b {
			return "true"
		}
		return "false"
	case kStr:
		return v.s
	case kNum:
		if v.n == float64(int64(v.n)) {
			return strconv.FormatInt(int64(v.n), 10)
		}
		return strconv.FormatFloat(v.n, 'f', -1, 64)
	default:
		return ""
	}
}

func (v value) truthy() bool {
	switch v.kind {
	case kBool:
		return v.b
	case kStr:
		return v.s != ""
	case kNum:
		return v.n != 0
	default:
		return false
	}
}

func strVal(s string) value  { return value{kind: kStr, s: s} }
func boolVal(b bool) value   { return value{kind: kBool, b: b} }
func numVal(n float64) value { return value{kind: kNum, n: n} }
func nullVal() value         { return value{kind: kNull} }

// equals implements GitHub-Actions-style loose equality where a bool may be
// compared against a truthy string/number.
func (v value) equals(o value) bool {
	if v.kind == o.kind {
		switch v.kind {
		case kBool:
			return v.b == o.b
		case kStr:
			return v.s == o.s
		case kNum:
			return v.n == o.n
		default:
			return true
		}
	}
	// Mixed-type: compare by truthiness, mirroring JS-like semantics.
	return v.truthy() == o.truthy()
}

// --- tokenizer ---

type tokenKind int

const (
	tEOF tokenKind = iota
	tStr
	tNum
	tIdent
	tOp // == != < <= > >= && || !
	tLParen
	tRParen
	tComma
)

type token struct {
	kind tokenKind
	val  string
}

//nolint:gocyclo
func tokenize(s string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '(':
			toks = append(toks, token{tLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tRParen, ")"})
			i++
		case c == ',':
			toks = append(toks, token{tComma, ","})
			i++
		case c == '\'':
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string literal")
			}
			toks = append(toks, token{tStr, s[i+1 : j]})
			i = j + 1
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string literal")
			}
			toks = append(toks, token{tStr, s[i+1 : j]})
			i = j + 1
		case c == '=' || c == '!' || c == '<' || c == '>' || c == '&' || c == '|':
			op := string(c)
			if i+1 < len(s) && (s[i+1] == '=' || s[i+1] == c && (c == '&' || c == '|')) {
				op += string(s[i+1])
				i += 2
			} else if op == "!" || op == "<" || op == ">" {
				i++
			} else {
				return nil, fmt.Errorf("unexpected character %q", c)
			}
			toks = append(toks, token{tOp, op})
		case c >= '0' && c <= '9':
			j := i
			for j < len(s) && (s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			toks = append(toks, token{tNum, s[i:j]})
			i = j
		case isIdentRune(c, true):
			j := i
			for j < len(s) && isIdentRune(s[j], j == i) {
				j++
			}
			toks = append(toks, token{tIdent, s[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	toks = append(toks, token{tEOF, ""})
	return toks, nil
}

// --- parser (recursive descent) ---

type parser struct {
	toks []token
	pos  int
}

func eval(in string, s Scope) (value, error) {
	toks, err := tokenize(in)
	if err != nil {
		return nullVal(), err
	}
	p := &parser{toks: toks}
	v, err := p.parseOr(s)
	if err != nil {
		return nullVal(), err
	}
	if p.peek().kind != tEOF {
		return nullVal(), fmt.Errorf("unexpected token %q", p.peek().val)
	}
	return v, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }

func (p *parser) parseOr(s Scope) (value, error) {
	left, err := p.parseAnd(s)
	if err != nil {
		return nullVal(), err
	}
	for p.peek().kind == tOp && p.peek().val == "||" {
		p.next()
		right, err := p.parseAnd(s)
		if err != nil {
			return nullVal(), err
		}
		left = boolVal(left.truthy() || right.truthy())
	}
	return left, nil
}

func (p *parser) parseAnd(s Scope) (value, error) {
	left, err := p.parseNot(s)
	if err != nil {
		return nullVal(), err
	}
	for p.peek().kind == tOp && p.peek().val == "&&" {
		p.next()
		right, err := p.parseNot(s)
		if err != nil {
			return nullVal(), err
		}
		left = boolVal(left.truthy() && right.truthy())
	}
	return left, nil
}

func (p *parser) parseNot(s Scope) (value, error) {
	if p.peek().kind == tOp && p.peek().val == "!" {
		p.next()
		v, err := p.parseNot(s)
		if err != nil {
			return nullVal(), err
		}
		return boolVal(!v.truthy()), nil
	}
	return p.parseCmp(s)
}

func (p *parser) parseCmp(s Scope) (value, error) {
	left, err := p.parsePrimary(s)
	if err != nil {
		return nullVal(), err
	}
	if p.peek().kind != tOp {
		return left, nil
	}
	op := p.next().val
	if op != "==" && op != "!=" && op != "<" && op != "<=" && op != ">" && op != ">=" {
		return nullVal(), fmt.Errorf("unexpected operator %q", op)
	}
	right, err := p.parsePrimary(s)
	if err != nil {
		return nullVal(), err
	}
	return compare(op, left, right)
}

func compare(op string, l, r value) (value, error) {
	switch op {
	case "==":
		return boolVal(l.equals(r)), nil
	case "!=":
		return boolVal(!l.equals(r)), nil
	case "<", "<=", ">", ">=":
		// Numeric or lexicographic comparison.
		var less bool
		if l.kind == kNum && r.kind == kNum {
			less = l.n < r.n
		} else {
			less = l.String() < r.String()
		}
		switch op {
		case "<":
			return boolVal(less), nil
		case "<=":
			return boolVal(less || l.equals(r)), nil
		case ">":
			return boolVal(!less && !l.equals(r)), nil
		case ">=":
			return boolVal(!less), nil
		}
	}
	return nullVal(), fmt.Errorf("bad operator %q", op)
}

func (p *parser) parsePrimary(s Scope) (value, error) {
	t := p.peek()
	switch t.kind {
	case tLParen:
		p.next()
		v, err := p.parseOr(s)
		if err != nil {
			return nullVal(), err
		}
		if p.peek().kind != tRParen {
			return nullVal(), fmt.Errorf("expected )")
		}
		p.next()
		return v, nil
	case tStr:
		p.next()
		return strVal(t.val), nil
	case tNum:
		p.next()
		n, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return nullVal(), err
		}
		return numVal(n), nil
	case tIdent:
		p.next()
		switch t.val {
		case "true":
			return boolVal(true), nil
		case "false":
			return boolVal(false), nil
		case "null":
			return nullVal(), nil
		}
		v, ok := s.Lookup(t.val)
		if !ok {
			return nullVal(), fmt.Errorf("unknown identifier %q", t.val)
		}
		return strVal(v), nil
	default:
		return nullVal(), fmt.Errorf("unexpected token %q", t.val)
	}
}
