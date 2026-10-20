// Package model holds types shared by the host and interpreted rules.
package model

type Order struct {
	ID       string
	Customer string
	Total    float64
	Items    int
}
