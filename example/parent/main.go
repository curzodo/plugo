package main

import (
	"github.com/curzodo/plugo"
	"time"
)

func main() {
	// Create a plugo with Id "Parent"
	p, _ := plugo.New("Parent")

	// Any plugos connected to this plugo will be able to call these functions.
	p.Expose(_reverse)
	p.Expose(_addToCount)

	// Start all plugos inside the 'plugos' folder, or create a folder with that
	// name if it does not exist. This function will block until all plugos signal
	// that they are ready, running this function on a goroutine will bypass the
	// blocking nature of this function, however.
	p.StartChildren("plugos")

	// Call remote  functions.
	resp, err := p.Call("Child", "_add", 2, 3)

	if err != nil {
		p.Println(err.Error())
		p.Shutdown()
		return
	}

	// Type assert any value into int.
	sum := resp[0].(int)

	p.Println("Result of calling _add is", sum)

	// Wait five seconds so that the child plugo can play around with the functions
	// this plugo has exposed, then shut down.
	time.Sleep(5 * time.Second)
	p.Shutdown()
}

// One should prefix all the functions one chooses to expose with an underscore.
// Exposed functions are technically public as they can be called from outside
// the current package, but not in the traditional sense. Therefore, it is good
// practice (in my opinion) to use an underscore to tell the reader that this
// function is exposed.

// Takes a string argument and returns the string reversed.
func _reverse(s string) string {
	runes := []rune(s)

	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}

	return string(runes)
}

var count int = 0

// This function adds the argument int to count and returns the new value of
// count. This function demonstrates that exposed functions can affect the
// environment of their host plugo and are not self-contained.
func _addToCount(num int) int {
	count += num
	return count
}
