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
    "github.com/bytedance/sonic"
)

const (
    // Datagram types.
    registration = byte(0)
    registrationResponse = byte(1)
    ready = byte(2)
    functionCall = byte(3)
    functionCallResponse = byte(4)

	// Error types.
	FunctionNotFound = byte(0)
    NoResponse = byte(1)

	network = "unix"
)

var (
    // If plugo A calls function f on plugo B, and before f has returned values
    // to plugo A, plugo A calls function g on plugo B, which returns values
    // instantly, then how will plugo A know which return values belong to
    // which request, given that all requests occur over the same connection?
    // The solution is a form of multiplexing using Ids and channels that wait
    // for return values from function call requests. Each connected plugo has
    // its own dedicated map of channels.
    functionResponseChannels = make(map[string]struct {
        sync.Mutex
        m map[int]chan []byte
    })
)

type Plugo struct {
	Id       string
	ParentId string
	exposedFunctions          map[string]reflect.Value
	listener                  *net.UnixListener
	connections               map[string]*net.UnixConn
    MemoryAllocation int
}

// Datagrams are used for inter-plugo communication. Depending on the type,
// the Payload will be unmarshalled into one of the structs defined below.
type datagram struct {
    Type byte
    Payload []byte
}

// This datagram is sent by a child plugo to its parent, over the connection that
// was passed as an argument to the program. The child sends its own Id.
type registrationRequest struct {
    PlugoId string
}

// This datagram is sent by a parent plugo to a child in response to a 
// registrationRequest datagram. The parent sents its own Id.
type registrationResponse struct {
    PlugoId string
}

type readySignal struct {
    FunctionNames []string
}

type functionCallRequest struct {
    FunctionCallRequestId int FunctionId string
    Arguments []byte
}

type functionCallResponse struct {
    FunctionCallRequestId int
    ReturnValues []byte
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
		listener,
		make(map[string]*net.UnixConn),
        4096,
	}

	// Check if this plugo was started by another plugo.
	if len(os.Args) < 2 {
		return &plugo, nil
	}

	// Get this plugo's parent's socket address.
	parentSocketAddress, err := net.ResolveUnixAddr(network, os.Args[2])

	if err != nil {
		return nil, err
	}

	// Create a connection with this plugo's parent.
	parentConnection, err := net.DialUnix(network, nil, parentSocketAddress)

	if err != nil {
		return nil, err
	}

    err := plugo.registerWithParent(parentConnection)

    if err != nil {
        return nil, err
    }

	return &plugo, nil
}

// This function handles the registration process between a child and a parent,
// from the child's point of view.
func (plugo *Plugo) registerWithParent(parentConnection *net.UnixConn) error {
    // Construct registration request datagram.
    payload := registrationRequest {
        plugo.id,
    }

    request := datagram {
        registration,
        payload,
    }

    // Marshal request into bytes.
    requestAsBytes, err := sonic.Marshal(&request)

    if err != nil {
        return err
    }

    // Send datagram to parent.
    parentConnection.Write(requestAsBytes)

    // Prepare for response.
    response := make([]byte, plugo.MemoryAllocation)

    // Set read deadline to one second.
    parentConnection.SetReadDeadline(time.Now().Add(time.Second()))

    // Await response.
    responseLength, err := parentConnection.Read(response)
    
    // Unset read deadline.
    parentConnection.SetReadDeadline(time.Time{})

    if err != nil {
        return err
    }

    // Unmarshal bytes into datagram.
    var datagram datagram
    err := sonic.Unmarshal(response[0:responseLength], &datagram)

    if err != nil {
        return err
    }

    if datagram.Id != registration {
        return errors.New(
            "The registration request response datagram's Id was "+
            "not correct. Received " + string(datagram.Id) +
            " instead of " + string(registration) + ".",
        )
    }

    // Unmarshal payload into registrationRequestResponse.
    var registrationRequestResponse registrationRequestResponse
    err := sonic.Unmarshal(datagram.Payload, &registrationRequestResponse)

    if err != nil {
        return err
    }
    
    // Store this plugo's parent's Id connection.
    parentId = registrationRequestResponse.plugoId
    plugo.parentId = parentId
    plugo.connections[parentId] = parentConnection

    return nil
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

func (plugo *Plugo) StartChildren(folderName string) error {
	// Check if folder already exists.
	_, err := os.Stat(folderName)

	// If the folder already exists, start all of the plugos inside.
	if err == nil {
		childPlugos, _ := os.ReadDir(folderName)

        if err != nil {
            return err
        }

        // Create a channel so that the plugo knows when the registration
        // process has been completed. This channel will consist of a map
        // of plugos who have requested certain functions be called. The
        // key is the plugo, and the associated value is an array of function
        // names to be called.
        done := make(chan map[string][]string)

        go plugo.handleRegistration(done)

		for _, childPlugo := range childPlugos {
			plugo.start(folderName + "/" + childPlugo.Name())
		}
        
        // Wait for map to be returned.
        mapPlugoIdFunctionName<-done 

        return nil
	}

	// If the error is that the folder does not exist, then create the folder.
	if errors.Is(err, os.ErrNotExist) {
		// Create folder
		os.Mkdir(folderName, os.ModePerm)
	} else if err != nil {
		return err
	}
}

func (plugo *Plugo) handleRegistration(done chan map[string][]string) {
    // Create a wait group to wait for each plugo that registers
    // to signal that it is ready.
    var waitGroup sync.Waitgroup

    functionCallRequests := make(map[string][]string)

    go func() {
    for {
        connection, err := plugo.listener.AcceptUnix()

        if err != nil {
            continue
        }

        go func() {
            datagramBytes := make([]byte, plugo.MemoryAllocation)

            // Listen for incoming registration requests over this connection.
            datagramBytesLength, err := connection.Read(datagramBytes)

            if err != nil {
                continue
            }

            // Unmarshal bytes into datagram.
            var datagram datagram
            err := sonic.Unmarshal(datagramBytes[0:datagramBytesLength], &datagram)

            if err != nil {
                continue
            }

            // Ensure datagram type is registration.
            if datagram.Type != registration {
                continue
            }

            // Unmarshal payload bytes into registrationRequest.
            var registrationRequest registrationRequest
            err := sonic.Unmarshal(datagram.Payload, &registrationRequest)

            if err != nil {
                continue
            }

            connectedPlugoId := registrationRequest.PlugoId
            plugo.connections[connectedPlugoId] = connection

            // Respond with registrationResponse.
            payload := registrationResponse {
                plugo.Id,
            }
            
            datagram := datagram {
                registrationResponse,
                payload,
            }

            connection.Write(datagram)

            // Add one to wait group.
            waitGroup.add(1)

            // Wait for ready signal.
            datagramBytes = make([]byte, plugo.MemoryAllocation)
            datagramBytesLength, err = connection.Read(datagramBytes)

            if err != nil {
                wg.Done()
                continue
            }

            // Unmarshal bytes into datagram.
            var datagram datagram
            err := sonic.Unmarshal(datagramBytes, &datagram)

            if err != nil {
                wg.Done()
                continue
            }

            // Check if datagram type is ready.
            if datagram.Type != ready {
                wg.Done()
                continue
            }

            // Unmarshal payload bytes into readySignal.
            var readySignal readySignal
            err := sonic.Unmarshal(datagram.Payload, &readySignal)

            if err != nil {
                wg.Done()
                continue
            }

            // Store function call requests.
            functionCallRequests[connectedPlugoId] = readySignal.FunctionNames

            // Store connectedPlugoId and connection..
            plugo.connections[connectedPlugoId] = connection

            wg.Done()
        }
    }
    }

    // Sleep for one hundred milliseconds to allow plugos to register.
    time.Sleep(100 * time.Millisecond)
    
    // Wait for all registered plugos to signal that they are ready.
    waitGroup.Wait()

    // Signal to main thread that the registration process is complete.
    done <- true
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
