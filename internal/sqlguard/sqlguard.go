// Package sqlguard tokenizes SQL and rejects anything that is not clearly
// read-only, so every integration exposing a query tool (ClickHouse,
// Databricks, Athena) enforces the same rule before a statement leaves the
// process. Each dialect supplies its own keyword sets: the tokenizer is
// dialect-independent, the vocabulary is not.
//
// This is a guardrail, not a parser. It complements — never replaces — a
// read-only database user or a read-only IAM policy.
package sqlguard

import (
	"fmt"
	"strings"
)

// Rules is one dialect's read-only vocabulary. Lead lists the verbs a
// statement may begin with; Mutating lists verbs rejected anywhere in a
// statement, so an allowed prefix (a `WITH … INSERT` CTE) cannot smuggle a
// write past the lead check. The Desc fields name the two sets in errors.
type Rules struct {
	Lead          map[string]bool
	Mutating      map[string]bool
	AllowedDesc   string
	ForbiddenDesc string
}

// Validate checks that every `;`-separated statement in sql begins with an
// allowed lead keyword and contains no mutating keyword, returning an error
// naming the first offender. SQL with no statement at all is rejected.
func (r Rules) Validate(sql string) error {
	sawStatement := false
	for _, toks := range Statements(sql) {
		if len(toks) == 0 {
			continue
		}
		sawStatement = true
		if !r.Lead[toks[0]] {
			return fmt.Errorf("statement starting with %q is not allowed; only read-only statements may run (%s)", toks[0], r.AllowedDesc)
		}
		for _, t := range toks {
			if r.Mutating[t] {
				return fmt.Errorf("statement contains the mutating keyword %q; %s and other write operations are forbidden", t, r.ForbiddenDesc)
			}
		}
	}
	if !sawStatement {
		return fmt.Errorf("no SQL statement found")
	}
	return nil
}

// Statements splits sql at top-level semicolons and returns the uppercased
// word tokens of each statement, ignoring anything inside string literals,
// quoted identifiers (`…`) and comments so they can't affect statement
// boundaries or keyword detection.
func Statements(sql string) [][]string {
	var (
		stmts [][]string
		cur   []string
		tok   strings.Builder
	)
	flushTok := func() {
		if tok.Len() > 0 {
			cur = append(cur, strings.ToUpper(tok.String()))
			tok.Reset()
		}
	}
	flushStmt := func() {
		flushTok()
		stmts = append(stmts, cur)
		cur = nil
	}

	i, n := 0, len(sql)
	for i < n {
		ch := sql[i]
		switch {
		case ch == '-' && i+1 < n && sql[i+1] == '-':
			// Line comment: skip to end of line.
			flushTok()
			i += 2
			for i < n && sql[i] != '\n' {
				i++
			}
		case ch == '/' && i+1 < n && sql[i+1] == '*':
			// Block comment: skip to closing */.
			flushTok()
			i += 2
			for i+1 < n && (sql[i] != '*' || sql[i+1] != '/') {
				i++
			}
			i += 2
		case ch == '\'' || ch == '"' || ch == '`':
			// Quoted span: string literal or quoted identifier. Skip its
			// contents. Handle doubled-quote escapes for all three and
			// backslash escapes inside string literals.
			flushTok()
			quote := ch
			backslashEscapes := quote == '\'' || quote == '"'
			i++
			for i < n {
				if backslashEscapes && sql[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if sql[i] == quote {
					if i+1 < n && sql[i+1] == quote {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case ch == ';':
			flushStmt()
			i++
		case (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_':
			tok.WriteByte(ch)
			i++
		case ch >= '0' && ch <= '9' && tok.Len() > 0:
			// A digit extends an identifier already started (e.g. t1); a digit
			// that starts a token is part of a number, which we skip.
			tok.WriteByte(ch)
			i++
		default:
			flushTok()
			i++
		}
	}
	flushStmt()
	return stmts
}
