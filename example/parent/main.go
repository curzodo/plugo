package main

import (
    "fmt"
    "github.com/curzodo/plugo"
)

func main() {
    // The plugo will create a folder named plugos into which we will drop our
    // executable child plugos or plugins for this application.
    p := plugo.New("Parent", 0)

    // Expose _Message() function to connected plugos.
    p.Expose("_Message", _Message)

    p.StartChildren("plugos")

    // Wait for three seconds, then call the _Add() function on the 
    // child plugin. Use the function to add the numbers two and three.
    returnValues, _ := p.CallWithTimeout("Child", "_Add", 1000, 2, 3)

    result := returnValues[0].(int)

    fmt.Println(
        "Result from calling _Add() function with arguments 2 and 3 :",
        result,
    )

    p.Shutdown()
}

func _Message(message string) {
    fmt.Println("Message from child plugo: ", message)
}
