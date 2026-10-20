//go:build ignore

package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

func main() {
	const workers, jobs = 8, 200000
	in := make(chan int, 128)
	var total atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range in {
				total.Add(int64(j % 7))
			}
		}()
	}
	for i := 0; i < jobs; i++ {
		in <- i
	}
	close(in)
	wg.Wait()
	var once sync.Once
	for i := 0; i < 3; i++ {
		once.Do(func() { fmt.Println("once") })
	}
	fmt.Println(total.Load())
}
