# Plugo
A plugin library for Go.

# Concept & Walkthrough
In this system, everything is a plugo. A plugo can be a parent,  a child, or both. A plugo that starts other plugos is considered a parent, and a plugo that is started by another plugo is considered a child. If one were to make a game, and then create a modding API for that game using the Plugo library, then the game program itself would be considered the parent plugo, and any mods would be considered children plugos. Because of the architecture of the Plugo library, those mods could also utilise the Plugo library and make themselves moddable.

Following on from the game analogy, the following chunk of code is what the game program might look like.
```
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

	// Shut down this plugo. Removes temporary files and
	// closes all connections.
	p.Shutdown()
}

func _Message(message string) {
	fmt.Println("Message from child plugo: ", message)
}
```

Obviously, an actual game program might choose to expose functions that meaningfully impact the game, such as function that sets the health of a player.

The mod program would then look like the following.
```
package main

import (
	"github.com/curzodo/plugo"
	"time"
)

func main() {
	// Create a plugo with the Id "Child".
	p, _ := plugo.New("Child")

	// Expose the add() function defined below.
	p.Expose("_Add", _Add)

	// Do some fake setting up.
	time.Sleep(5 * time.Second)

	// Signal to the parent plugo that this plugo is ready.
	p.Ready()

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
		time.Sleep(5 * time.Second)
	}
}

func _Add(x, y int) int {
	return x + y
}
```

The mod program is built into an executable file using ```go build``` and then this exxecutable file is dragged into whatever folder you set the parent plugo to search for children in. If you download the repo and navigate into the example/parent directory and run the command ```go run .```, then you can see this code in action.

# Limitations
Currently, exposed functions can only return booleans, integers, float32, float64, and string types. The same applies for arguments.

# Motivation
I created this library because I wanted a fast and simple library to incorporate a plugin system to another one of my projects. I explored other options such as [gRPC](https://https://grpc.io/), Hashicorp's [go-plugin](https://github.com/hashicorp/go-plugin) and Go's very own [plugin](https://pkg.go.dev/plugin), but all of these solutions had drawbacks too significant to ignore, namely, the absurd amount of boilerplate involved, or lack of bi-directional function calling (the Plugo library allows parents to call functions on children, and children to call functions on parents).

# Use of ```any```
The use of ```any``` to retrieve return values from remotely called functions is not ideal, but as far as I am aware is necessary to bypass the need for boilerplate. Typically, the Plugo library should not be exposed to users of a plugin API, but instead exposed to functions that wrap around Plugo library functions. An example of this is my [Jukebox](https://github.com/orgs/jukebox-mc/repositories) project, a Minecraft server that supports  plugins/mods written in Go. At the time of writing, Jukebox is still using an older version of Plugo.
