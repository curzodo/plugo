# Plugo
A plugin library for Go, designed with ease of use and minimal boilerplate in mind.

# How does it work?
This library implements plugins as separate processes. Both the host process and
the plugin process(es) are abstracted into what this library defines as 'plugos'.
A plugo is a server that communicates with other plugos in order to provide the
features of a traditional plugin system such as those available in Java. Plugos
are built into executables using `go build` and then dragged into a folder within
their parent's directory. The name of this folder depends on the name specified by
the developer of the parent plugo. See the example/parent/plugos folder.

# Working example
There is a working, thoroughly commented example of plugos in the /example
directory. The parent plugo is the host process, and the child plugo is 
its plugin. The child has been compiled into an executable using `go build` 
and copied into the the parent's 'plugos' folder, where it's started by the parent.
The parent can be run by running `go run .`, and its interactions with the child
plugo will be printed out in the terminal.

# Limitations
Any values that can be encoded and decoded with encoding/gob can be sent as
arguments to, or returned as values by, an exposed function. This excludes
structs. Error types cannot be encoded as types using encoding/gob, however,
if the last returned value of an exposed function is of type error, then it 
can be received by the calling plugo in the error value returned by p.Call(),
p.CallWithContext() and p.CallWithTimeout().

# Planned features
- Secure communication between plugos using encryption
- Exposing functions via structs
- Sending/receiving structs to/from functions as arguments/return values

# Comments
If your intent is to use this library to create a plugin system for a Go
project so that other developers can extend that Go project, then I highly
suggest that you do not directly expose your developers to Plugo, but instead
create wrapper functions around plugo functions and have your plugin developers
use those. An example of this is my own project, [Redstone](https://github.com/orgs/redstone-mc/repositories). Redstone is a Minecraft
server that supports plugins written in Go, however, the plugo library is
abstracted away by what I call the Disc API, so from a plugin developer's
perspective, the code they write is being executed on the main process directly.
