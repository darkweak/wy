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

// Wide returns hi<<32 + lo: generated code passes the halves of an integer
// constant that is negative on some platforms and does not fit int64 on
// others.
func Wide(hi, lo int64) constant.Value {
	return constant.BinaryOp(constant.Shift(constant.MakeInt64(hi), token.SHL, 32), token.ADD, constant.MakeInt64(lo))
}
