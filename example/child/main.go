package main

import (
	"github.com/curzodo/plugo"
	"time"
    "fmt"
)

func main() {
	// Create a plugo with the Id "Child".
	p, err := plugo.New("Child")

    fmt.Println(err)

	// Expose the _add() function defined below.
	p.Expose(_add)

	// Do some fake setting up.
	time.Sleep(time.Second)

	// Signal to the parent plugo that this plugo is ready.
	p.Ready("")

	// Call the Message() function defined in the Parent plugo.
	// p.CallWithTimeout("Parent", "_message", 1000, "Hi Parent")

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

func _add(x, y int) int {
	return x + y
}
