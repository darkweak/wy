// Command rules shows wy embedded in a host program: business rules are
// written in plain Go, loaded at run time, and come back as native,
// statically typed functions that share the host's types.
package main

import (
	"context"
	"fmt"
	"log"

	"wy/examples/rules/model"
	"wy/interp"
)

// Rules are ordinary Go source, e.g. read from a file or a database.
const rules = `
import "wy/examples/rules/model"

var vip = map[string]bool{"ada": true}

func discount(o model.Order) float64 {
	d := 0.0
	if o.Total > 100 { d += 0.05 }
	if vip[o.Customer] { d += 0.10 }
	return math.Round(o.Total*d*100) / 100
}
`

func main() {
	ctx := context.Background()
	s, err := interp.New(interp.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Eval(ctx, rules); err != nil {
		log.Fatal(err)
	}
	v, err := s.Get(ctx, "discount")
	if err != nil {
		log.Fatal(err)
	}
	discount := v.(func(model.Order) float64) // a real Go func, no wrappers

	orders := []model.Order{
		{ID: "A1", Customer: "ada", Total: 250, Items: 3},
		{ID: "B2", Customer: "bob", Total: 80, Items: 1},
	}
	for _, o := range orders {
		fmt.Printf("%s: total %.2f, discount %.2f\n", o.ID, o.Total, discount(o))
	}

	// Host values go in with their static types, too.
	if err := s.Set(ctx, "orders", orders); err != nil {
		log.Fatal(err)
	}
	vals, err := s.Eval(ctx, `
sum := 0.0
for _, o := range orders { sum += o.Total - discount(o) }
sum`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("revenue after discounts:", vals[0])

	// Rules can be changed while the program runs.
	if _, err := s.Eval(ctx, `vip["bob"] = true`); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("B2 discount once bob is VIP: %.2f\n", discount(orders[1]))
}
