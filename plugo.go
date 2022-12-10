package plugo

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

const (
	// These bytes prefix the datagrams sent between plugos.
	registrationByte         = byte(1)
	readyByte                = byte(2)
	functionCallByte         = byte(3)
	functionCallResponseByte = byte(4)

	// Error bytes
	functionNotFoundByte = byte(5)

	network = "unix"
)

var (
	// These byte arrays are used to encode and decode datagrams.
	delimiterZero  = []byte{0, 0, 0, 0, 0, 0, 0, 0, 1}
	delimiterOne   = []byte{1, 1, 1, 1, 1, 1, 1, 1, 1}
	delimiterTwo   = []byte{2, 2, 2, 2, 2, 2, 2, 2, 2}
	delimiterThree = []byte{3, 3, 3, 3, 3, 3, 3, 3, 3}

	// When one plugo calls several functions on another almost simultaneously,
	// the calling plugo needs to be able to return the correct return values to
	// the correct function call request, all on the same connection. This is
	// accomplished using identifiers and channels which wait for called functions
	// responses encoded into bytes. Think of it as multiplexing a single pipeline
	// in order to achieve multiple connections with just a single connection.
	channels = struct {
		sync.Mutex
		m map[int][]chan any
	}{
		sync.Mutex{},
		make(map[int][]chan any),
	}
)

type Plugo struct {
	id       string
	parentId string
	// ExposedFunctions will be called using reflect.Call(), whereas
	// FastExposedFunctions will be called directly, but they must be
	// of type func(any...) []any
	exposedFunctions          map[string]reflect.Value
	exposedFunctionsNoReflect map[string]func(...any) []any
	listener                  *net.UnixListener
	connections               map[string]*net.UnixConn
}

// This function creates a plugo. plugoId should
// be unique to avoid conflicts with other plugos.
func New(plugoId string) (*Plugo, error) {
	// Create a unix socket for this plugo.
	socketDirectoryName, err := os.MkdirTemp("", "tmp")

	if err != nil {
		return nil, err
	}

	// Use the Id of this plugo to create the unix socket address.
	socketAddressString := socketDirectoryName + "/" + plugoId + ".sock"
	socketAddress, err := net.ResolveUnixAddr(network, socketAddressString)

	if err != nil {
		return nil, err
	}

	// Useful for local testing.
	// print(socketAddress.String())

	// Create the listener.
	listener, err := net.ListenUnix(network, socketAddress)

	if err != nil {
		return nil, err
	}

	// Instantiate the plugo struct.
	plugo := Plugo{
		plugoId,
		"",
		make(map[string]reflect.Value),
		make(map[string]func(...any) []any),
		listener,
		make(map[string]*net.UnixConn),
	}

	// Check if this plugo was started by another plugo.
	if len(os.Args) < 3 {
		return &plugo, nil
	}

	// Get this plugo's parent's socket address.
	parentSocketAddress, err := net.ResolveUnixAddr(network, os.Args[2])

	if err != nil {
		return nil, err
	}

	// Create a connection with this plugo's parent.
	connection, err := net.DialUnix(network, nil, parentSocketAddress)

	if err != nil {
		return nil, err
	}

	// Set the connection deadline to given time.
	connection.SetReadDeadline(time.Now().Add(time.Second))

	// Send this plugo's Id, prefixed by the
	// registration byte, to its parent.
	datagram := append([]byte{registrationByte}, []byte(plugoId)...)

	connection.Write(datagram)

	// Expect a single registration byte as a response.
	response := make([]byte, 1)
	_, err = connection.Read(response)

	if err != nil {
		return nil, err
	}

	// Unset the connection read deadline.
	connection.SetReadDeadline(time.Time{})

	if response[0] != registrationByte {
		return nil, errors.New(
			"This plugo could not register with its parent.",
		)
	}

	// Store this plugo's parent's Id.
	plugo.parentId = os.Args[1]

	// Store this connection with the other plugo's Id.
	plugo.connections[plugo.parentId] = connection

	// Handle connection for future communications.
	go plugo.handleConnection(plugo.parentId, connection)

	return &plugo, nil
}

// This function send informs the parent plugo that this
// plugo has finished setting up and that the parent
// plugo should continue its execution.
func (plugo *Plugo) Ready() error {
	connection, ok := plugo.connections[plugo.parentId]

	if !ok {
		return errors.New(
			"Parent plugo connection could not be found.",
		)
	}

	connection.Write([]byte{readyByte})

	return nil
}

func (plugo *Plugo) StartChildren(folderName string) {
	// Check if folder already exists.
	_, err := os.Stat(folderName)

	// If the folder already exists, start all of the plugos inside.
	if err == nil {
		childPlugos, _ := os.ReadDir(folderName)

		for _, childPlugo := range childPlugos {
			plugo.start(folderName + "/" + childPlugo.Name())
		}

		plugo.handleRegistrations()
	}

	// If the folder does not exist, then create the folder.
	if errors.Is(err, os.ErrNotExist) {
		// Create folder
		os.Mkdir(folderName, os.ModePerm)
	} else if err != nil {
		panic(err)
	}
}

// This function starts and attempts to connect to another plugo.
// Path is the path to an executable.
func (plugo *Plugo) start(path string) {
	cmd := exec.Command(path, plugo.id, plugo.listener.Addr().String())
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
	cmd.Start()
}

// This function exposes the given function to connected plugos. If f is of
// type func(...any) []any it will be not be called using reflect, resulting
// in better performance. functionId is the Id through which other
// plugos may call the function using the plugo.Call() function.
func (plugo *Plugo) Expose(functionId string, function any) {
	functionType := reflect.TypeOf(function).String()

	if functionType == "func(...interface {}) []interface {}" {
		plugo.exposedFunctionsNoReflect[functionId] = function.(func(...any) []any)
		return
	}

	plugo.exposedFunctions[functionId] = reflect.ValueOf(function)
}

// This function unexposes the function with the given functionId.
func (plugo *Plugo) Unexpose(functionId string) {
	delete(plugo.exposedFunctions, functionId)
	delete(plugo.exposedFunctionsNoReflect, functionId)
}

// This function gracefully shuts the plugo down. It stops listening
// for incoming connections and closes all existing connections.
func (plugo *Plugo) Shutdown() {
	// Obtain socket address from listener.
	socketAddress := plugo.listener.Addr().String()

	// Close listener.
	plugo.listener.Close()

	// Close all stored connections.
	for _, connection := range plugo.connections {
		connection.Close()
	}

	// Delete temporary directory.
	directoryPath := filepath.Dir(socketAddress)
	os.Remove(directoryPath)
}

func (plugo *Plugo) CheckConnection(plugoId string) bool {
	_, ok := plugo.connections[plugoId]
	return ok
}

// This function remotely calls functions exposed by connected plugos.
// plugoId is the name of the connected plugo whose exposed function
// should be called. functionId is the Id of the function that should
// be called (not necessarily the function name). ctx is a context
// that can be used to construct a timeout. arguments are what the
// remote function will be passed as arguments.
func (plugo *Plugo) CallWithContext(
	plugoId string,
	functionId string,
	ctx context.Context,
	arguments ...any,
) ([]any, error) {
	// Check if a connection with key plugoId exists.
	connection, ok := plugo.connections[plugoId]

	if !ok {
		return nil, errors.New(
			"A plugo with Id " + plugoId + " is not connected to this plugo.",
		)
	}

	// Construct value and error channels.
	valueChannel := make(chan any, 1)
	errorChannel := make(chan any, 1)

	// Lock the channels map, find available datagram Id (key).
	channels.Lock()

	datagramId := 0
	for {
		_, ok := channels.m[datagramId]

		if !ok {
			channels.m[datagramId] = []chan any{valueChannel, errorChannel}
			channels.Unlock()
			break
		}

		datagramId++
	}

	// Encode arguments into bytes.
	encodedArguments := encode(arguments...)

	// Construct datagram for function call request.
	datagram := make([]byte, 0, 4096)

	// Append functionCallByte to let receiver know the purpose of this
	// datagram.
	datagram = append(datagram, functionCallByte)

	// Append datagramId so that when the data is sent back it is sent
	// to the correct channel.
	datagram = append(datagram, byte(datagramId))

	// Append Id of the function we are calling.
	datagram = append(datagram, []byte(functionId)...)

	// Append delimiter separating function Id and arguments.
	datagram = append(datagram, delimiterOne...)

	// Append encoded arguments.
	datagram = append(datagram, encodedArguments...)

	connection.Write(datagram)

	select {
	// Case where the return values of the remote function are returned.
	case returnValues := <-valueChannel:
		delete(channels.m, datagramId)
		return returnValues.([]any), nil
	// Case where an error is received in place of return values.
	case err := <-errorChannel:
		delete(channels.m, datagramId)
		return nil, err.(error)
	// Case where no response is heard back from the plugo whose function
	// was remotely called.
	case <-ctx.Done():
		delete(channels.m, datagramId)
		return nil, errors.New(
			"This context given to this function has been cancelled.",
		)
	}
}

// This function remotely calls functions exposed by connected plugos.
// plugoId is the name of the connected plugo whose exposed function
// should be called. functionId is the Id of the function that should
// be called (not necessarily the function name). timeout is the
// amount of time in milliseconds the function will wait for a reply
// from the called remote function, and arguments are what the remote
// function will be passed as arguments.
func (plugo *Plugo) CallWithTimeout(
	plugoId string,
	functionId string,
	timeout int,
	arguments ...any,
) ([]any, error) {
	ctx, _ := context.WithDeadline(
		context.Background(),
		time.Now().Add(time.Duration(timeout)*time.Millisecond),
	)

	returnValues, err := plugo.CallWithContext(
		plugoId,
		functionId,
		ctx,
		arguments...,
	)

	return returnValues, err
}

// This function handles the registration and connection of plugos to this
// plugo. Any connections made within one second of this function being
// called will have the ability to force this plugo to wait for an
// additional ready signal before continuing program execution.
func (plugo *Plugo) handleRegistrations() {
	var waitGroup sync.WaitGroup

	go func() {
		for {
			connection, err := plugo.listener.AcceptUnix()

			if err != nil {
				continue
			}

			// Start a thread to handle the new incoming connection.
			go func() {
				// Set the connection deadline to one second.
				connection.SetReadDeadline(time.Now().Add(time.Second))

				datagram := make([]byte, 4096)
				datagramLength, err := connection.Read(datagram)

				if err != nil {
					connection.Close()
					return
				}

				// Unset the connection read deadline.
				connection.SetReadDeadline(time.Time{})

				// Check if this datagram is prefixed with a registration byte.
				if datagram[0] != registrationByte {
					connection.Close()
					return
				}

				// Extract plugoId from datagram.
				childPlugoId := string(datagram[1:datagramLength])

				// Add one to wait group.
				waitGroup.Add(1)

				// Write registration byte back, and add the child plugo's Id and
				// connection to this plugo's array of connections.
				connection.Write([]byte{registrationByte})

				// Wait for ready byte.
				datagram = make([]byte, 1)
				_, err = connection.Read(datagram)

				if err != nil {
					connection.Close()
					return
				}

				if datagram[0] != readyByte {
					connection.Close()
					return
				}

				// Store this connection with the other plugo's Id.
				plugo.connections[childPlugoId] = connection

				// Handle this connection for future communications.
				go plugo.handleConnection(childPlugoId, connection)

				waitGroup.Done()
			}()
		}
	}()

	// Wait one second for all initial registrations to take place.
	time.Sleep(time.Second)

	// Wait for all registered plugos to send ready signal.
	waitGroup.Wait()
}

// This function handles connections between plugos.
func (plugo *Plugo) handleConnection(
	connectedPlugoId string,
	connection *net.UnixConn,
) {
	for {
		datagram := make([]byte, 4096)
		datagramSize, err := connection.Read(datagram)

		if err != nil {
			delete(plugo.connections, connectedPlugoId)
			break
		}

		datagram = datagram[0:datagramSize]

		// Create a new thread to handle this datagram.
		go func() {
			// Datagram will be prefixed with one of three bytes.
			switch datagram[0] {
			case functionCallByte:
				plugo.handleFunctionCall(connection, datagram[1:])
			case functionCallResponseByte:
				plugo.handleFunctionCallResponse(connection, datagram[1:])
			default:
			}
		}()
	}
}

// This function handles datagrams that are prefixed with the functionCallByte.
func (plugo *Plugo) handleFunctionCall(
	connection *net.UnixConn,
	datagram []byte,
) {
	// Store this datagram's Id, we will use the same Id in the response.
	datagramId := datagram[0]

	// Create response datagram.
	responseDatagram := make([]byte, 0, 4096)

	// Append functionCallResponseByte to responseDatagram.
	responseDatagram = append(responseDatagram, functionCallResponseByte)

	// Append datagram Id to responseDatagram.
	responseDatagram = append(responseDatagram, datagramId)

	// Split remaining bytes into functionId and argument(s) (type-value).
	datagramSplit := bytes.Split(datagram[1:], delimiterOne)
	functionIdBytes := datagramSplit[0]
	argumentBytes := datagramSplit[1]

	functionId := string(functionIdBytes)
	arguments := decode(argumentBytes)

	// Check if function is in ExposedFunctions or FastExposedFunctions.
	functionWithReflect, ok := plugo.exposedFunctions[functionId]

	if ok {
		// If function exists in ExposedFunctions, then call it using reflect.
		returnValues := callWithReflect(functionWithReflect, arguments)

		// Encode return values into bytes.
		returnValueBytes := encode(returnValues...)

		// Append return values' bytes to datagram.
		responseDatagram = append(responseDatagram, returnValueBytes...)

		connection.Write(responseDatagram)
	} else {
		// Otherwise, call function without using reflect.
		functionNoReflect, ok := plugo.exposedFunctionsNoReflect[functionId]

		// If the function cannot be found, then return a datagram with the error.
		if !ok {
			// Append error byte.
			responseDatagram = append(responseDatagram, functionNotFoundByte)
			connection.Write(datagram)
			return
		}

		// Call function with arguments.
		returnValues := functionNoReflect(arguments)

		// Encode return values into bytes.
		returnValueBytes := encode(returnValues...)

		// Append return values' bytes to datagram
		responseDatagram = append(responseDatagram, returnValueBytes...)

		connection.Write(responseDatagram)
	}

}

func (plugo *Plugo) handleFunctionCallResponse(
	connection *net.UnixConn,
	datagram []byte,
) {
	// Extract datagram Id.
	datagramId := int(datagram[0])

	// Obtain channel waiting for return values.
	valueChannel := channels.m[datagramId][0]
	errorChannel := channels.m[datagramId][1]

	// If the length of the datagram is exactly two at this point, then the
	// second byte is an error byte as a result of something going wrong in
	// the function call request.
	if len(datagram) == 2 {
		errByte := datagram[1]

		var err error

		switch errByte {
		case functionNotFoundByte:
			err = errors.New(
				"The called function could not be found on the parent plugo.",
			)
		default:
			err = errors.New("Unknown error has occurred.")
		}

		errorChannel <- err
		return
	}

	// Convert bytes into values.
	values := decode(datagram[1:])

	valueChannel <- values
}

// This function calls the given function using reflect.Call().
func callWithReflect(function reflect.Value, arguments []any) []any {
	// Convert arguments to type reflect.Value
	argumentsAsReflectValues := make([]reflect.Value, len(arguments))

	for i, argument := range arguments {
		argumentsAsReflectValues[i] = reflect.ValueOf(argument)
	}

	reflectValues := function.Call(argumentsAsReflectValues)

	values := make([]any, len(reflectValues))

	for i := 0; i < len(values); i++ {
		values[i] = reflectValues[i].Interface()
	}

	return values
}

// This function encodes values into (type-value) byte array pairs.
func encode(values ...any) []byte {
	encodedValues := make([]byte, 0, 64)

	for _, value := range values {
		// Determine the type of the value.
		valueType := reflect.TypeOf(value)

		// Convert type of the value into bytes
		valueTypeBytes := []byte(valueType.Kind().String())

		numberOfBytes := int(valueType.Size())
		valueBytes := make([]byte, numberOfBytes)

		// Depending on the type of the argument, it will be encoded
		// a certain way.
		switch valueType.Kind() {
		case reflect.Bool:
			if value.(bool) == true {
				valueBytes[0] = byte(1)
			} else {
				valueBytes[0] = byte(0)
			}

		case reflect.Int:
			// Encode in little endian format.
			for i := 0; i < numberOfBytes; i++ {
				valueBytes[i] = byte(value.(int) >> (i * 8))
			}

		// Utilise math functions to convert floats to bits/bytes.
		// Makes my life very easy :D
		case reflect.Float32:
			floatAsBits := math.Float32bits(value.(float32))

			for i := 0; i < numberOfBytes; i++ {
				valueBytes[i] = byte(floatAsBits >> (i * 8))
			}

		case reflect.Float64:
			floatAsBits := math.Float64bits(value.(float64))

			for i := 0; i < numberOfBytes; i++ {
				valueBytes[i] = byte(floatAsBits >> (i * 8))
			}

		case reflect.String:
			valueBytes = []byte(value.(string))
		default:
			continue
		}

		// Append the value's type bytes.
		encodedValues = append(encodedValues, valueTypeBytes...)

		// Append delimiter separating argument type bytes from argument bytes.
		encodedValues = append(encodedValues, delimiterThree...)

		// Append the argument's bytes.
		encodedValues = append(encodedValues, valueBytes...)

		// Append the delimiter separating each (valueType-value) pair.
		encodedValues = append(encodedValues, delimiterTwo...)
	}

	// Remove the last delimiterTwo.
	if len(values) > 0 {
		encodedValues = encodedValues[:len(encodedValues)-9]
	}

	return encodedValues
}

func decode(valuesAsBytes []byte) []any {
	if len(valuesAsBytes) == 0 {
		return []any{}
	}

	// Return value.
	decodedValues := make([]any, 0, 3)

	// Split the bytes into (type-value) pairs.
	typeAndValuePairs := bytes.Split(valuesAsBytes, delimiterTwo)

	for _, typeAndValuePair := range typeAndValuePairs {
		// Split the pair into type bytes and value bytes.
		typeAndValuePairSplit := bytes.Split(typeAndValuePair, delimiterThree)
		typeBytes := typeAndValuePairSplit[0]
		valueBytes := typeAndValuePairSplit[1]

		// Determine the value's type.
		valueType := string(typeBytes)

		// Convert valueBytes into value depending on valueType.
		switch valueType {
		case reflect.Bool.String():
			decodedValues = append(decodedValues, valueBytes[0] != 0)
		case reflect.Int.String():
			var value int = 0
			for i, b := range valueBytes {
				value += int(b) << (i * 8)
			}

			decodedValues = append(decodedValues, value)

		case reflect.Float32.String():
			var value uint32 = 0

			for i, b := range valueBytes {
				value += uint32(b) << (i * 8)
			}

			decodedValues = append(decodedValues, math.Float32frombits(value))

		case reflect.Float64.String():
			var value uint64 = 0

			for i, b := range valueBytes {
				value += uint64(b) << (i * 8)
			}

			decodedValues = append(decodedValues, math.Float64frombits(value))

		case reflect.String.String():
			decodedValues = append(decodedValues, string(valueBytes))
		}
	}

	return decodedValues
}
