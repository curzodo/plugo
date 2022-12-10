package main

import (
	"fmt"
	"github.com/curzodo/plugo"
)

func main() {
	// Create a plugo with Id "Parent"
	p, _ := plugo.New("Parent")

	// Expose _Message() function to connected plugos.
	p.Expose("_Message", _Message)

	// Start all plugos inside a folder named "plugos",
	// or create a folder named "plugos" if none exists.
	p.StartChildren("plugos")

    // Call the _PrintFromChild() function. The child's
    // fmt.Println() call should appear in this process'
    // terminal output.
    p.CallWithTimeout("Child", "_PrintFromChild", 1000)

	// Call the _Add() function present on our child plugo.
	returnValues, _ := p.CallWithTimeout("Child", "_Add", 1000, 2, 3)

	// returnValues is an array, which we know contains a
	// single integer value because that is the output
	// of the _Add() function. Type assert back to int.
	result := returnValues[0].(int)

	fmt.Println(
		"Result from calling _Add() function with arguments 2 and 3 :",
		result,
	)

    // select{}

	// Shut down this plugo. Removes temporary files and
	// closes all connections.
	p.Shutdown()
}

func _Message(message string) {
	fmt.Println("Message from child plugo: ", message)
}
