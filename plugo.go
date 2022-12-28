package plugo

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

func init() {
	log.SetFlags(0)
}

const (
	network = "unix"
)

var (
	datagramTypes = struct {
		registrationRequest  byte
		registrationResponse byte
		readySignal          byte
		functionCallRequest  byte
		functionCallResponse byte
	}{
		byte(0),
		byte(1),
		byte(2),
		byte(3),
		byte(4),
	}

	// If plugo A calls function f on plugo B, and before f has returned values
	// to plugo A, plugo A calls function g on plugo B, which returns values
	// instantly, then how will plugo A know which return values belong to
	// which request, given that all requests occur over the same connection?
	// The solution is a form of multiplexing using Ids and channels that wait
	// for return values from function call requests. Each connected plugo has
	// its own dedicated map of channels.
	rcs = make(map[*net.UnixConn]returnChannels)
)

type Plugo struct {
	Id               string
	ParentId         string
	Functions        map[string]reflect.Value
	TypedFunctions   map[string]func(...any) []any
	Connections      map[string]*net.UnixConn
	Listener         *net.UnixListener
	MemoryAllocation int
}

type returnChannels struct {
	sync.Mutex
	m map[int]returnChannelsInner
}

type returnChannelsInner struct {
	valueChannel chan []any
	errorChannel chan error
}

// Datagrams are used for inter-plugo communication. Depending on the type,
// the Payload will be unmarshalled into one of the structs defined below.
type datagram struct {
	Type    byte
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

// This datagram is sent by a child to a parent to let the parent know
// that it has completed setting up and that the parent should continue
// execution. It sends an array of function names that the parent should
// call on the child.
type readySignal struct {
	FunctionId       string
	ExposedFunctions []string
}

type functionCallRequest struct {
	RequestId  int
	FunctionId string
	Arguments  []any
}

type functionCallResponse struct {
	ResponseId   int
	ReturnValues []any
	Error        string
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
		make(map[string]func(...any) []any),
		make(map[string]*net.UnixConn),
		listener,
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

	err = plugo.registerWithParent(parentConnection)

	if err != nil {
		return nil, err
	}

	return &plugo, nil
}

// This function handles the registration process between a child and a parent,
// from the child's point of view.
func (plugo *Plugo) registerWithParent(parentConnection *net.UnixConn) error {
	// Construct registration request datagram.
	payload := registrationRequest{
		plugo.Id,
	}

	payloadBytes, err := marshal(payload)

	if err != nil {
		return err
	}

	datagram := datagram{
		datagramTypes.registrationRequest,
		payloadBytes,
	}

	// Marshal datagram into bytes.
	datagramBytes, err := marshal(datagram)

	if err != nil {
		return err
	}

	// Send datagram to parent.
	parentConnection.Write(datagramBytes)

	// Prepare for response.
	response := make([]byte, plugo.MemoryAllocation)

	// Set read deadline to one second.
	parentConnection.SetReadDeadline(time.Now().Add(time.Second))

	// Await response.
	responseLength, err := parentConnection.Read(response)

	// Unset read deadline.
	parentConnection.SetReadDeadline(time.Time{})

	if err != nil {
		return err
	}

	// Unmarshal bytes into datagram.
	err = unmarshal(response[0:responseLength], &datagram)

	if err != nil {
		return err
	}

	if datagram.Type != datagramTypes.registrationResponse {
		return errors.New(
			"The registration request response datagram's Id was " +
				"not correct. Received " + string(datagram.Type) +
				" instead of " + string(datagramTypes.registrationRequest) + ".",
		)
	}

	// Unmarshal payload into registrationRequestResponse.
	var registrationResponse registrationResponse
	err = unmarshal(datagram.Payload, &registrationResponse)

	if err != nil {
		return err
	}

	// Store this plugo's parent's Id and connection.
	parentId := registrationResponse.PlugoId
	plugo.ParentId = parentId
	plugo.Connections[parentId] = parentConnection

	// Initialise return channel for remote plugo connection.
	rcs[parentConnection] = returnChannels{
		sync.Mutex{},
		make(map[int]returnChannelsInner),
	}

	// Handle future communications with the remote plugo on a separate goroutine.
	go plugo.handleConnection(parentId, parentConnection)

	return nil
}

func (plugo *Plugo) handleConnection(
	connectedPlugoId string,
	connection *net.UnixConn,
) {
	for {
		// Allocate space for incoming datagrams.
		datagramBytes := make([]byte, plugo.MemoryAllocation)

		// Read incoming datagrams.
		datagramLength, err := connection.Read(datagramBytes)

		// Re-slice datagram bytes based on actual length.
		datagramBytes = datagramBytes[0:datagramLength]

		// If there is an error with this connection, cleanup connection
		// and terminate for loop.
		if err != nil {
			plugo.Disconnect(connectedPlugoId)
			break
		}

		// Handle incoming datagram bytes on separate goroutine.
		go func() {
			// Unmarshal datagram.
			var datagram datagram
			err := unmarshal(datagramBytes, &datagram)

			if err != nil {
				return
			}

			// Process payload depending on datagram type.
			switch datagram.Type {
			case datagramTypes.functionCallRequest:
				plugo.handleFunctionCallRequest(connection, datagram.Payload)
			case datagramTypes.functionCallResponse:
				handleFunctionCallResponse(connection, datagram.Payload)
			default:
			}
		}()
	}
}

func (plugo *Plugo) handleFunctionCallRequest(
	connection *net.UnixConn,
	payload []byte,
) {
	// Unmarshal payload bytes into functionCall struct.
	var functionCallRequest functionCallRequest
	err := unmarshal(payload, &functionCallRequest)

	if err != nil {
		return
	}

	// Construct functionCallResponse struct.
	functionCallResponse := functionCallResponse{
		functionCallRequest.RequestId,
		[]any{},
		"",
	}

	function, ok := plugo.TypedFunctions[functionCallRequest.FunctionId]

	if ok {
		returnValues := function(functionCallRequest.Arguments)

		// If the last argument returned is an error, then send it in the
		// response as an error string.
		if len(returnValues) > 0 {
			err, ok = returnValues[len(returnValues)-1].(error)

			if ok {
				functionCallResponse.Error = err.Error()
			} else {
				functionCallResponse.ReturnValues = returnValues
			}
		}
	} else {
		reflectFunction, ok := plugo.Functions[functionCallRequest.FunctionId]

		if !ok {
			functionCallResponse.Error = "Function with Id " +
				functionCallRequest.FunctionId +
				" could not be found."
		} else {
			// Convert arguments of type any to arguments of type reflect.Value.
			var arguments = make(
				[]reflect.Value,
				len(functionCallRequest.Arguments),
			)

			for i, argument := range functionCallRequest.Arguments {
				arguments[i] = reflect.ValueOf(argument)
			}

			reflectReturnValues := reflectFunction.Call(arguments)

			// Convert return values to type any.
			returnValues := make([]any, len(reflectReturnValues))

			for i, reflectReturnValue := range reflectReturnValues {
				returnValues[i] = reflectReturnValue.Interface()
			}

			// If the last argument returned is an error, then send it in the
			// response as an error string.
			if len(returnValues) > 0 {
				err, ok = returnValues[len(returnValues)-1].(error)

				if ok {
					functionCallResponse.Error = err.Error()
				} else {
					functionCallResponse.ReturnValues = returnValues
				}
			}
		}
	}

	functionCallResponseBytes, err := marshal(functionCallResponse)

	if err != nil {
		return
	}

	datagram := datagram{
		datagramTypes.functionCallResponse,
		functionCallResponseBytes,
	}

	datagramBytes, err := marshal(datagram)

	if err != nil {
		return
	}

	connection.Write(datagramBytes)
}

func handleFunctionCallResponse(connection *net.UnixConn, payload []byte) {
	// Unmarshal payload into functionCallResponse struct.
	var functionCallResponse functionCallResponse
	err := unmarshal(payload, &functionCallResponse)

	if err != nil {
		return
	}

	responseId := functionCallResponse.ResponseId

	// Get value and error channels associated with the response's Id
	responseReturnChannels := rcs[connection].m[responseId]

	// Find out if there was an error.
	errorString := functionCallResponse.Error

	if errorString != "" {
		// If there was an error, then turn the error string into an
		// error and send it down the error channel for this request.
		responseReturnChannels.errorChannel <- errors.New(errorString)
		return
	}

	responseReturnChannels.valueChannel <- functionCallResponse.ReturnValues
}

func (plugo *Plugo) Call(
	plugoId string,
	functionId string,
	arguments ...any,
) ([]any, error) {
	returnValues, err := plugo.CallWithContext(
		plugoId,
		functionId,
		context.Background(),
		arguments...,
	)

	if err != nil {
		return []any{}, err
	}

	return returnValues, nil
}

// This function attempts to call a function with Id functionId exposed by a
// plugo with Id plugoId with arguments.
func (plugo *Plugo) CallWithContext(
	plugoId,
	functionId string,
	ctx context.Context,
	arguments ...any,
) ([]any, error) {
	// Check if this plugo is connected to a plugo with Id plugoId.
	connection, ok := plugo.Connections[plugoId]

	if !ok {
		return []any{}, errors.New(
			"This plugo is not connected to a plugo with Id " + plugoId + ".",
		)
	}

	// Create channel that will wait for the remotely called function's return
	// value(s).
	valueChannel := make(chan []any)

	// Create a channel that will wait for an error from the remote function.
	errorChannel := make(chan error)

	// Get data necessary for function call request datagram.

	// Find available datagramId in the returnChannels map for the plugo to who
	// the remote function call is being made.
	connectionReturnChannels := rcs[connection]
	connectionReturnChannels.Lock()

	datagramId := 0

	for {
		_, ok := connectionReturnChannels.m[datagramId]

		if !ok {
			connectionReturnChannels.m[datagramId] = returnChannelsInner{
				valueChannel,
				errorChannel,
			}

			connectionReturnChannels.Unlock()
			break
		}

		datagramId++
	}

	// Construct payload.
	payload := functionCallRequest{
		datagramId,
		functionId,
		arguments,
	}

	payloadBytes, err := marshal(payload)

	if err != nil {
		return []any{}, err
	}

	// Construct datagram.
	datagram := datagram{
		datagramTypes.functionCallRequest,
		payloadBytes,
	}

	// Marshal datagram.
	datagramBytes, err := marshal(datagram)

	if err != nil {
		return []any{}, err
	}

	// Send datagram to plugo.
	connection.Write(datagramBytes)

	// The remote function's return value bytes will be sent into the
	// channel that was created a couple of lines up, so wait for something
	// to appear in that.
	select {
	case returnValues := <-valueChannel:
		delete(connectionReturnChannels.m, datagramId)
		return returnValues, nil

	case <-ctx.Done():
		delete(connectionReturnChannels.m, datagramId)
		return []any{}, errors.New(
			"The context/timeout for this call has been cancelled/reached.",
		)

	case err = <-errorChannel:
		delete(connectionReturnChannels.m, datagramId)
		return []any{}, err
	}
}

func (plugo *Plugo) CallWithTimeout(
	plugoId string,
	functionId string,
	milliseconds int,
	arguments ...any,
) ([]any, error) {
	ctx, _ := context.WithDeadline(
		context.Background(),
		time.Now().Add(time.Duration(milliseconds)*time.Millisecond),
	)

	returnValues, err := plugo.CallWithContext(
		plugoId,
		functionId,
		ctx,
		arguments...,
	)

	return returnValues, err
}

func (plugo *Plugo) Ready(functionId string) error {
	connection, ok := plugo.Connections[plugo.ParentId]

	if !ok {
		return errors.New(
			"Parent plugo connection could not be found.",
		)
	}

	// Send a list of all the functions this plugo has
	// exposed to the parent.
	exposedFunctions := make([]string, 0, 64)

	for key, _ := range plugo.Functions {
		exposedFunctions = append(exposedFunctions, key)
	}

	for key, _ := range plugo.TypedFunctions {
		exposedFunctions = append(exposedFunctions, key)
	}

	readySignal := readySignal{
		functionId,
		exposedFunctions,
	}

	readySignalBytes, err := marshal(readySignal)

	if err != nil {
		return err
	}

	datagram := datagram{
		datagramTypes.readySignal,
		readySignalBytes,
	}

	datagramBytes, err := marshal(datagram)

	if err != nil {
		return err
	}

	connection.Write(datagramBytes)

	return nil
}

func (plugo *Plugo) StartChildren(folderName string) (map[string][]string, error) {
	// Check if folder already exists.
	_, err := os.Stat(folderName)

	// If the folder already exists, start all of the plugos inside.
	if err == nil {
		childPlugos, err := os.ReadDir(folderName)

		if err != nil {
			return nil, err
		}

		startChildren := func() {
			for _, childPlugo := range childPlugos {
				plugo.start(folderName + "/" + childPlugo.Name())
			}
		}

		done := plugo.handleRegistration(startChildren)

		// Wait for registration process to complete.
		exposedFunctions := <-done

		return exposedFunctions, nil
	}

	// If the error is that the folder does not exist, then create the folder.
	if errors.Is(err, os.ErrNotExist) {
		// Create folder
		os.Mkdir(folderName, os.ModePerm)
	} else if err != nil {
		return nil, err
	}

	return make(map[string][]string), nil
}

func (plugo *Plugo) handleRegistration(startChildren func()) chan map[string][]string {
	// Create a wait group to wait for each plugo that registers
	// to signal that it is ready.
	var waitGroup sync.WaitGroup

	functionCallRequests := make(map[string]string)
	exposedFunctions := make(map[string][]string)

	go func() {
		for {
			connection, err := plugo.Listener.AcceptUnix()

			if err != nil {
				continue
			}

			// New goroutine for each connection.
			go func() {
				datagramBytes := make([]byte, plugo.MemoryAllocation)

				// Listen for incoming registration requests over this connection.
				datagramBytesLength, err := connection.Read(datagramBytes)

				if err != nil {
					return
				}

				// Unmarshal bytes into datagram.
				var datagram datagram

				err = unmarshal(
					datagramBytes[0:datagramBytesLength],
					&datagram,
				)

				if err != nil {
					return
				}

				// Ensure datagram type is registration.
				if datagram.Type != datagramTypes.registrationRequest {
					return
				}

				// Unmarshal payload bytes into registrationRequest.
				var registrationRequest registrationRequest
				err = unmarshal(datagram.Payload, &registrationRequest)

				if err != nil {
					return
				}

				connectedPlugoId := registrationRequest.PlugoId
				plugo.Connections[connectedPlugoId] = connection

				// Respond with registrationResponse.
				payload := registrationResponse{
					plugo.Id,
				}

				payloadBytes, err := marshal(payload)

				if err != nil {
					return
				}

				datagram.Type = datagramTypes.registrationResponse
				datagram.Payload = payloadBytes

				datagramBytes, err = marshal(datagram)

				if err != nil {
					return
				}

				connection.Write(datagramBytes)

				// Add one to wait group.
				waitGroup.Add(1)

				// Wait for ready signal.
				datagramBytes = make([]byte, plugo.MemoryAllocation)
				datagramBytesLength, err = connection.Read(datagramBytes)

				if err != nil {
					return
				}

				// Unmarshal bytes into datagram.
				err = unmarshal(datagramBytes[:datagramBytesLength], &datagram)

				if err != nil {
					return
				}

				// Check if datagram type is ready.
				if datagram.Type != datagramTypes.readySignal {
					return
				}

				// Unmarshal payload bytes into readySignal.
				var readySignal readySignal
				err = unmarshal(datagram.Payload, &readySignal)

				if err != nil {
					return
				}

				// Store function call requests.
				if readySignal.FunctionId != "" {
					functionCallRequests[connectedPlugoId] = readySignal.FunctionId
				}

				exposedFunctions[connectedPlugoId] = readySignal.ExposedFunctions

				// Store connectedPlugoId and connection..
				plugo.Connections[connectedPlugoId] = connection

				// Log that this plugo has successfully connected.
				plugo.Println(
					registrationRequest.PlugoId,
					"has successfully connected.",
				)

				// Initialise channel map for this plugo's connection.
				rcs[connection] = returnChannels{
					sync.Mutex{},
					make(map[int]returnChannelsInner),
				}

				go plugo.handleConnection(connectedPlugoId, connection)

				waitGroup.Done()
			}()
		}
	}()

	// Start children plugos after 10ms.
	time.Sleep(5 * time.Millisecond)
	startChildren()

	// Sleep for one hundred milliseconds to allow plugos to register.
	time.Sleep(100 * time.Millisecond)

	done := make(chan map[string][]string)

	go func() {
		// Wait for all registered plugos to signal that they are ready.
		waitGroup.Wait()

		// Call all requested functions. Wait for responses.
		for plugoId, functionId := range functionCallRequests {
			if functionId == "" {
				continue
			}

			_, err := plugo.Call(plugoId, functionId)

			if err != nil {
				plugo.Println(err.Error())
			}
		}

		// Signal to main thread that the registration process is complete.
		done <- exposedFunctions
	}()

	return done
}

// This function starts and attempts to connect to another plugo.
// Path is the path to an executable.
func (plugo *Plugo) start(path string) {
	cmd := exec.Command(path, plugo.Id, plugo.Listener.Addr().String())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Start()
}

// This function exposes the given function to connected plugos. If f is of
// type func(...any) []any it will be not be called using reflect, resulting
// in better performance. functionId is the Id through which other
// plugos may call the function using the plugo.Call() function.
func (plugo *Plugo) Expose(function any) {
	reflectValue := reflect.ValueOf(function)

	split := strings.Split(
		runtime.FuncForPC(reflectValue.Pointer()).Name(), ".",
	)

	functionId := split[len(split)-1]

	functionType := reflect.TypeOf(function).String()

	if functionType == "func(...interface {}) []interface {}" {
		plugo.TypedFunctions[functionId] = function.(func(...any) []any)
		return
	}

	plugo.Functions[functionId] = reflect.ValueOf(function)
}

// Exposes the function just like Expose(), but uses the given Id.
func (plugo *Plugo) ExposeId(functionId string, function any) {
	functionType := reflect.TypeOf(function).String()

	if functionType == "func(...interface {}) []interface {}" {
		plugo.TypedFunctions[functionId] = function.(func(...any) []any)
		return
	}

	plugo.Functions[functionId] = reflect.ValueOf(function)
}

// This function unexposes the function with the given functionId.
func (plugo *Plugo) Unexpose(functionId string) {
	delete(plugo.Functions, functionId)
	delete(plugo.TypedFunctions, functionId)
}

// This function gracefully shuts the plugo down. It stops listening
// for incoming connections and closes all existing connections.
func (plugo *Plugo) Shutdown() {
	// Obtain socket address from listener.
	socketAddress := plugo.Listener.Addr().String()

	// Close all stored connections.
	for connectedPlugoId, _ := range plugo.Connections {
		plugo.Disconnect(connectedPlugoId)
	}

	// Close listener.
	plugo.Listener.Close()

	// Delete temporary directory.
	directoryPath := filepath.Dir(socketAddress)
	os.Remove(directoryPath)
}

func (plugo *Plugo) CheckConnection(connectedPlugoId string) bool {
	_, ok := plugo.Connections[connectedPlugoId]
	return ok
}

func (plugo *Plugo) Disconnect(plugoId string) {
	connection, ok := plugo.Connections[plugoId]

	if !ok {
		return
	}

	// Remove connection from connection array.
	delete(plugo.Connections, plugoId)

	// For each return channel, return an error.
	for _, returnChannelsInner := range rcs[connection].m {
		errorChannel := returnChannelsInner.errorChannel

		errorChannel <- errors.New(
			"The connection associated with this function call is closed.",
		)
	}

	delete(rcs, connection)

	// Close connection.
	connection.Close()
}

func (plugo *Plugo) Println(args ...any) {
	args = append([]any{plugo.Id + ":"}, args...)
	log.Println(args...)
}

func marshal(v interface{}) ([]byte, error) {
	b := new(bytes.Buffer)
	err := gob.NewEncoder(b).Encode(v)

	if err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func unmarshal(data []byte, v interface{}) error {
	b := bytes.NewBuffer(data)

	return gob.NewDecoder(b).Decode(v)
}
