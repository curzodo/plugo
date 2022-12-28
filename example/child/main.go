package main

import (
	"fmt"
	"github.com/curzodo/plugo"
	"time"
)

func main() {
	// Create a plugo with the Id "Child".
	p, err := plugo.New("Child")

	if err != nil {
		fmt.Println(err.Error())
		return
	}

	// Expose some functions.
	p.Expose(_init)
	p.Expose(_add)

	// Do some fake setting up.
	time.Sleep(time.Second)

	// Signal to the parent plugo that this plugo is ready. Tell it to call the
	// _init() function once all plugos have signalled that they are ready.
	// Note that there is no guarantee that _init() will run before any of the
	// code below. One could give _init() a channel that it sends a value down
	// in order to let the main program (here) know that _init() has been
	// called, if one wished to block the execution of this program until
	// _init() has been called.
	p.Ready("_init")

	// Call remote functions.
	reverseArg := "Hello, world."
	resp, err := p.Call("Parent", "_reverse", reverseArg)

	if err != nil {
		p.Println(err.Error())
		p.Shutdown()
		return
	}

	// Type assert back into string, because we know that the underlying type of
	// the any value is a string, based on the return type of the exposed function.
	result := resp[0].(string)
	p.Println("Result of calling _reverse on '"+reverseArg+"':", result)

	resp, err = p.Call("Parent", "_addToCount", 3)

	if err != nil {
		p.Println(err.Error())
		p.Shutdown()
		return
	}

	count := resp[0].(int)
	p.Println("Current value of count is", count)

	resp, err = p.Call("Parent", "_addToCount", 3)

	if err != nil {
		p.Println(err.Error())
		p.Shutdown()
		return
	}

	count = resp[0].(int)
	p.Println("Current value of count is", count)

	// Create a loop that tests the connection with the parent plugo.
	// When the CheckConnection() function returns false, then the parent is
	// no longer alive/responsive and this plugo should be shut down.
	for {
		parentAlive := p.CheckConnection("Parent")

		if !parentAlive {
			p.Shutdown()
			break
		}

		// Check once every five seconds.
		time.Sleep(5 * time.Second)
	}
}

func _init() {
	fmt.Println("Hello, I'm the _init() function defined on the child.")
}

func _add(x, y int) int {
	return x + y
}
