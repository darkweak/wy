//go:build ignore

package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func producer(ctx context.Context, out chan<- int) {
	defer close(out)
	for i := 0; ; i++ {
		select {
		case out <- i:
		case <-ctx.Done():
			return
		}
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan int)
	go producer(ctx, ch)
	sum := 0
	for v := range ch {
		sum += v
		if v == 100 {
			cancel()
			break
		}
	}
	fmt.Println("sum", sum)

	results := make(chan string, 3)
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- name + name
		}()
	}
	wg.Wait()
	close(results)
	got := map[string]bool{}
	for r := range results {
		got[r] = true
	}
	fmt.Println(len(got), got["aa"], got["cc"])

	timeout := time.After(50 * time.Millisecond)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	n := 0
	func() {
		for {
			select {
			case <-tick.C:
				n++
			case <-timeout:
				return
			}
		}
	}()
	fmt.Println("ticked", n > 5)

	var mu sync.Mutex
	counter := 0
	var wg2 sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			mu.Lock()
			counter++
			mu.Unlock()
		}()
	}
	wg2.Wait()
	fmt.Println("counter", counter)

	done := make(chan struct{})
	var once sync.Once
	for i := 0; i < 3; i++ {
		go once.Do(func() { close(done) })
	}
	<-done
	v, ok := <-func() chan int { c := make(chan int); close(c); return c }()
	fmt.Println("closed recv", v, ok)
	select {
	case x := <-make(chan int):
		fmt.Println("impossible", x)
	default:
		fmt.Println("default case")
	}
}
