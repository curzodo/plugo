package main

import (
	"fmt"
	"github.com/curzodo/plugo"
)

func main() {
	// Create a plugo with Id "Parent"
	p, _ := plugo.New("Parent")

	// Expose _message() function to connected plugos.
	p.Expose(_message)

	// Start all plugos inside a folder named "plugos",
	// or create a folder named "plugos" if none exists.
	p.StartChildren("plugos")

	// Call the _Add() function present on our child plugo.
    var x int = 2
    var y int = 3
	returnValues, err := p.Call("Child", "_add", x, y)

    if err != nil {
        fmt.Println(err)
        return
    }

	// returnValues is an array, which we know contains a
	// single integer value because that is the output
	// of the _Add() function. Type assert back to int.
	result := returnValues[0].(int)

	fmt.Println(
		"Result from calling _add() function with arguments 2 and 3:",
		result,
	)

	// Shut down this plugo. Removes temporary files and
	// closes all connections.
	p.Shutdown()
}

func _message(message string) {
	fmt.Println("Message from child plugo: ", message)
}
