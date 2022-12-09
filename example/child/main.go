package main

import (
	"github.com/curzodo/plugo"
	"time"
)

func main() {
    // Give an empty string for folderName because we know that this plugo will
    // not have any children of its own.
	p := plugo.New("Child", 5000)

    // Expose the add() function defined below.
    p.Expose("_Add", _Add)

    // Call the Message() function defined in the Parent plugo.
	p.CallWithTimeout("Parent", "_Message", 1000, "Hi Parent")

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
        time.Sleep(time.Duration(5) * time.Second)
    }
}

func _Add(x, y int) int {
	return x + y
}
