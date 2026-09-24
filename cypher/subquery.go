package cypher

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cloudprivacylabs/opencypher/parser"
)

// subqueryPlaceholderPrefix names the synthetic zero-arg procedure call
// substituted for each "CALL { ... }" block before the text reaches the
// ANTLR parser. The vendored cloudprivacylabs/opencypher grammar has no
// rule for the "CALL { subquery }" form at all — only for
// "CALL <procedure>(...)" (a stored-procedure call) — and that runtime is
// frozen (see AGENTS.md's antlr4-go version-lock note), so there is no
// grammar-level fix available. buildCallSubqueryClause recognises this
// prefix and substitutes the corresponding pre-parsed subquery back in.
const subqueryPlaceholderPrefix = "__graphlite_subquery_"

// extractCallSubqueries scans input for top-level "CALL {" blocks, using
// string-literal-aware brace matching (a '{' or '}' inside a quoted string
// does not affect the balance), and returns:
//   - rewritten: input with each "CALL { ... }" replaced by a synthetic
//     "CALL __graphlite_subquery_<N>()" call, which the ANTLR grammar
//     already accepts as an ordinary procedure-call reading clause.
//   - inner: the raw text found inside each pair of braces, in encounter
//     order (inner[N] corresponds to placeholder N).
//
// Only top-level (non-nested) blocks are matched in this pass; a "CALL {}"
// nested inside another one is left untouched in its extracted inner text —
// Parse calls this function again on that text, so nested subqueries are
// handled by ordinary recursion rather than anything special here.
func extractCallSubqueries(input string) (rewritten string, inner []string, err error) {
	var b strings.Builder
	i := 0
	n := len(input)
	for i < n {
		c := input[i]
		if c == '\'' || c == '"' {
			end, err := skipStringLiteral(input, i)
			if err != nil {
				return "", nil, err
			}
			b.WriteString(input[i:end])
			i = end
			continue
		}
		if isCallKeywordAt(input, i) {
			j := skipInlineWhitespace(input, i+4)
			if j < n && input[j] == '{' {
				braceEnd, err := matchBrace(input, j)
				if err != nil {
					return "", nil, err
				}
				idx := len(inner)
				inner = append(inner, input[j+1:braceEnd])
				fmt.Fprintf(&b, "CALL %s%d()", subqueryPlaceholderPrefix, idx)
				i = braceEnd + 1
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), inner, nil
}

// isCallKeywordAt reports whether input[i:] begins with the case-insensitive
// keyword "CALL" at a word boundary (so it doesn't match inside a longer
// identifier like "callback").
func isCallKeywordAt(input string, i int) bool {
	const kw = "call"
	if i+len(kw) > len(input) || !strings.EqualFold(input[i:i+len(kw)], kw) {
		return false
	}
	if i > 0 && isIdentChar(input[i-1]) {
		return false
	}
	if i+len(kw) < len(input) && isIdentChar(input[i+len(kw)]) {
		return false
	}
	return true
}

func isIdentChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func skipInlineWhitespace(input string, i int) int {
	for i < len(input) {
		switch input[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipStringLiteral returns the index just past the string literal starting
// at input[start] (which must be a quote character), honouring the same
// backslash-escape convention as unquoteString so an escaped quote does not
// end the literal early.
func skipStringLiteral(input string, start int) (int, error) {
	quote := input[start]
	i := start + 1
	for i < len(input) {
		if input[i] == '\\' {
			i += 2 // skip the escaped character too
			continue
		}
		if input[i] == quote {
			return i + 1, nil
		}
		i++
	}
	return 0, fmt.Errorf("cypher: unterminated string literal")
}

// matchBrace returns the index of the '}' matching the '{' at input[open],
// honouring nested braces and skipping over string literals along the way.
func matchBrace(input string, open int) (int, error) {
	depth := 0
	i := open
	for i < len(input) {
		c := input[i]
		if c == '\'' || c == '"' {
			end, err := skipStringLiteral(input, i)
			if err != nil {
				return 0, err
			}
			i = end
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
		i++
	}
	return 0, fmt.Errorf("cypher: unterminated CALL {} block: missing closing '}'")
}

// buildCallSubqueryClause converts an OC_InQueryCallContext produced by our
// synthetic "CALL __graphlite_subquery_N()" placeholder back into a
// CallSubqueryClause wrapping the pre-parsed inner *Query. A genuine
// procedure call (e.g. "CALL db.labels()") is not supported and returns a
// clear error naming the procedure, rather than being silently misread as a
// subquery.
func buildCallSubqueryClause(ctx *parser.OC_InQueryCallContext, subqueries []*Query) (*CallSubqueryClause, error) {
	epiCtx := ctx.OC_ExplicitProcedureInvocation()
	if epiCtx == nil {
		return nil, fmt.Errorf("cypher: unsupported CALL syntax %q", ctx.GetText())
	}
	epi, ok := epiCtx.(*parser.OC_ExplicitProcedureInvocationContext)
	if !ok {
		return nil, fmt.Errorf("cypher: unsupported CALL syntax %q", ctx.GetText())
	}
	name := trimWhitespace(epi.OC_ProcedureName().GetText())
	idxText, ok := strings.CutPrefix(name, subqueryPlaceholderPrefix)
	if !ok {
		return nil, fmt.Errorf("cypher: CALL <procedure> is not supported (got %q) — only CALL { subquery } is supported", name)
	}
	n, err := strconv.Atoi(idxText)
	if err != nil || n < 0 || n >= len(subqueries) {
		return nil, fmt.Errorf("cypher: internal error: invalid subquery placeholder %q", name)
	}
	return &CallSubqueryClause{Inner: subqueries[n]}, nil
}
