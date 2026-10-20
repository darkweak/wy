package native

import (
	"go/constant"
	"go/token"
	"strings"
)

// Tokens used by generated constant expressions.
const (
	TokenINT = token.INT
	TokenADD = token.ADD
)

// Float parses an exact floating-point constant as printed by
// constant.Value.ExactString: a decimal literal or a "num/den" fraction.
func Float(s string) constant.Value {
	if i := strings.IndexByte(s, '/'); i > 0 {
		num := constant.MakeFromLiteral(strings.TrimSpace(s[:i]), token.INT, 0)
		den := constant.MakeFromLiteral(strings.TrimSpace(s[i+1:]), token.INT, 0)
		return constant.BinaryOp(num, token.QUO, den)
	}
	return constant.MakeFromLiteral(s, token.FLOAT, 0)
}
