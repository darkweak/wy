package tree

import (
	"reflect"
	"strconv"
	"strings"
)

// %T in printing functions
//
// fmt prints %T from the operand's reflect type, which for an interpreted
// type is its underlying structure (or a wrapper). In printing calls, the
// format string is rewritten so that each %T operand is replaced by the
// type name the interpreter knows (main.point). Every verb is given an
// explicit argument index, so operands can be substituted without
// changing fmt's argument numbering.

// rewriteTypeVerbs returns a format and operands with %T verbs resolved.
// names(i) returns the type name of operand i.
func rewriteTypeVerbs(format string, nargs int, names func(i int) string) (string, []string, bool) {
	if !strings.Contains(format, "T") {
		return format, nil, false
	}
	var b strings.Builder
	var extra []string
	changed := false
	argNum := 0
	i := 0
	for i < len(format) {
		c := format[i]
		if c != '%' {
			b.WriteByte(c)
			i++
			continue
		}
		start := i
		i++
		if i < len(format) && format[i] == '%' {
			b.WriteString("%%")
			i++
			continue
		}
		var spec strings.Builder
		spec.WriteByte('%')
		// flags
		for i < len(format) && strings.IndexByte("#0+- ", format[i]) >= 0 {
			spec.WriteByte(format[i])
			i++
		}
		// explicit index before width
		readIndex := func() bool {
			if i < len(format) && format[i] == '[' {
				j := strings.IndexByte(format[i:], ']')
				if j < 0 {
					return false
				}
				n, err := strconv.Atoi(format[i+1 : i+j])
				if err != nil || n < 1 {
					return false
				}
				argNum = n - 1
				i += j + 1
			}
			return true
		}
		if !readIndex() {
			b.WriteString(format[start:])
			return b.String(), extra, changed
		}
		// width
		if i < len(format) && format[i] == '*' {
			spec.WriteString("[" + strconv.Itoa(argNum+1) + "]*")
			argNum++
			i++
		} else {
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				spec.WriteByte(format[i])
				i++
			}
		}
		// precision
		if i < len(format) && format[i] == '.' {
			spec.WriteByte('.')
			i++
			if !readIndex() {
				b.WriteString(format[start:])
				return b.String(), extra, changed
			}
			if i < len(format) && format[i] == '*' {
				spec.WriteString("[" + strconv.Itoa(argNum+1) + "]*")
				argNum++
				i++
			} else {
				for i < len(format) && format[i] >= '0' && format[i] <= '9' {
					spec.WriteByte(format[i])
					i++
				}
			}
		}
		if !readIndex() {
			b.WriteString(format[start:])
			return b.String(), extra, changed
		}
		if i >= len(format) {
			b.WriteString(format[start:])
			break
		}
		verb := format[i]
		i++
		if verb == 'T' && argNum < nargs {
			extra = append(extra, names(argNum))
			spec.WriteString("[" + strconv.Itoa(nargs+len(extra)) + "]s")
			changed = true
		} else {
			spec.WriteString("[" + strconv.Itoa(argNum+1) + "]")
			spec.WriteByte(verb)
		}
		argNum++
		b.WriteString(spec.String())
	}
	return b.String(), extra, changed
}

// printfTypeNames rewrites a printf-style call's format and operands (args
// are interpreter values of static type any; fi is the format's index).
func (in *Interp) printfTypeNames(args []reflect.Value, fi int) ([]reflect.Value, bool) {
	if fi < 0 || fi >= len(args) || args[fi].Kind() != reflect.String {
		return args, false
	}
	ops := args[fi+1:]
	if len(ops) == 1 && ops[0].Kind() == reflect.Slice {
		// spread call: f(format, xs...)
		s := ops[0]
		ops = make([]reflect.Value, s.Len())
		for j := range ops {
			ops[j] = s.Index(j)
		}
		if s.Type().Elem() != anyRT {
			return args, false
		}
	}
	format, extra, changed := rewriteTypeVerbs(args[fi].String(), len(ops), func(i int) string {
		v := ops[i]
		if v.Kind() != reflect.Interface {
			return v.Type().String()
		}
		nt, _ := in.dyn(v)
		if nt == nil {
			return "<nil>"
		}
		return nt.name
	})
	if !changed {
		return args, false
	}
	out := append([]reflect.Value{}, args[:fi]...)
	out = append(out, reflect.ValueOf(format))
	slice := reflect.MakeSlice(reflect.SliceOf(anyRT), 0, len(ops)+len(extra))
	for _, o := range ops {
		slice = reflect.Append(slice, o)
	}
	for _, e := range extra {
		slice = reflect.Append(slice, reflect.ValueOf(e).Convert(anyRT))
	}
	return append(out, slice), true
}
