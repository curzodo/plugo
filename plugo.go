package plugo

import (
	"bytes"
	"errors"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"
)

const (
	network = "unix"

	// Registration byte
	rByte = byte(1)

	// Function call byte
	fcByte = byte(2)

	// Function call response byte
	fcrByte = byte(3)
)

var (
	// Termination channel
	termc = make(chan os.Signal, 1)

	// Delimiter to separate function call array
	delimOne = []byte{1, 1, 1, 1, 1, 1, 1, 1, 1}

	// Delimiter to separate function name from value-type array
	delimTwo = []byte{2, 2, 2, 2, 2, 2, 2, 2, 2}

	// Delimiter to separate value-type array
	delimThree = []byte{3, 3, 3, 3, 3, 3, 3, 3, 3}

	// Delimiter to separate value from type
	delimFour = []byte{4, 4, 4, 4, 4, 4, 4, 4, 4}
)

var channels = struct {
	sync.Mutex
	m map[int]chan []byte
}{
	sync.Mutex{},
	make(map[int]chan []byte),
}

func New(Id string) Plugo {
	socketAddr, err := createSocket(Id)

	if err != nil {
		panic(err)
	}

	ln, err := net.ListenUnix(network, socketAddr)

	if err != nil {
		panic(err)
	}

	plugo := Plugo{
		Id,
		"",
		ln,
		make(map[string]FuncSig),
		make(map[string]*net.UnixConn),
	}

	// Creates a goroutine that waits for the ctrl+c signal
	go plugo.cleanup()

    // Expose 'alive' function, child plugos use this function to determine if parent is alive
    plugo.Expose("__alive__", func(_ ...any) []any {
        return []any{true}
    })

	// If there is more than one program argument, then this is a child plugo
	if len(os.Args) > 1 {
		// Register with parent plugo
		parentSocketAddr, err := net.ResolveUnixAddr(network, os.Args[2])

		if err != nil {
			panic(err)
		}

		conn, err := net.DialUnix(network, nil, parentSocketAddr)

		if err != nil {
			panic(err)
		}

		conn.Write(append([]byte{rByte}, []byte(Id)...))

		response := make([]byte, 1)
		_, err = conn.Read(response)

		if err != nil {
			panic(err)
		}

		if response[0] != rByte {
			panic("Could not register with parent plugo properly")
		}

		plugo.Conns[os.Args[1]] = conn

		go plugo.handleConn(conn)
	}

	go plugo.handleNewConn()

	return plugo
}

func (plugo *Plugo) StartChild(path string) {
	cmd := exec.Command(path, plugo.Id, plugo.Ln.Addr().String())
	cmd.Start()
}

func (plugo *Plugo) Expose(functionName string, function FuncSig) {
	plugo.Exposed[functionName] = function
}

func (plugo *Plugo) handleNewConn() {
	for {
		conn, err := plugo.Ln.AcceptUnix()

		if err != nil {
			continue
		}

		go func() {
			data := make([]byte, 64)
			num, err := conn.Read(data)

			if err != nil || data[0] != rByte {
				return
			}

			go plugo.handleConn(conn)

			conn.Write([]byte{rByte})

			plugo.Conns[string(data[1:num])] = conn
		}()
	}
}

func (plugo *Plugo) handleConn(conn *net.UnixConn) {
	datagram := make([]byte, 1024)

	for {
		if conn == nil {
			break
		}

		num, err := conn.Read(datagram)

		if err != nil {
			continue
		}

		go plugo.handleDatagram(datagram[:num], conn)
	}
}

func (plugo *Plugo) handleDatagram(datagram []byte, conn *net.UnixConn) {
	datagramType := datagram[0]

	switch datagramType {
	case fcByte:
		// Split bytes into function name and function args
		fc := bytes.Split(datagram[2:], delimTwo)
		function, ok := plugo.Exposed[string(fc[0])]

		if !ok {
			return
		}

		args := decode(fc[1])

		fcr := function(args...)

		response := append([]byte{fcrByte}, datagram[1])
		response = append(response, encode(fcr...)...)
		conn.Write(response)
	case fcrByte:
		channels.m[int(datagram[1])] <- datagram[2:]
	default:
	}
}

func (plugo *Plugo) Call(plugoId, functionName string, args ...any) ([]any, error) {
	conn, ok := plugo.Conns[plugoId]

	if !ok {
		return nil, errors.New("Plugo with name " + plugoId + " could not be found in connections map")
	}

	// Channel will block until there is a response (or timeout)
	c := make(chan []byte, 256)

	// Lock the channel map
	channels.Lock()

	// Find an available key in the channel map
	key := 0
	for {
		_, ok := channels.m[key]

		if !ok {
			channels.m[key] = c
			channels.Unlock()
			break
		}

		key++
	}

	request := append([]byte{fcByte}, byte(key))

	request = append(request, []byte(functionName)...)

	request = append(request, delimTwo...)

	encodedArgs := encode(args...)

	request = append(request, encodedArgs...)

	conn.Write(request)

	select {
	case response := <-c:
		delete(channels.m, key)
		return decode(response), nil
	case <-time.After(time.Second):
		delete(channels.m, key)
		return nil, errors.New("Received no response from remote entity")
	}
}

func createSocket(Id string) (*net.UnixAddr, error) {
	dirName, err := os.MkdirTemp("", "tmp")

	if err != nil {
		return nil, err
	}

	// Use the Id of the plugo to create the unix socket
	socketAddr, err := net.ResolveUnixAddr(network, dirName+"/"+Id+".sock")

	if err != nil {
		return nil, err
	}

	return socketAddr, nil
}

// This function handles the cleanup of the unix socket whose address
// was passed as an argument.
func (plugo *Plugo) cleanup() {
	signal.Notify(termc, syscall.SIGINT)

	<-termc

	// Stop listener
	plugo.Ln.Close()

	// Close all connections
	for _, conn := range plugo.Conns {
		conn.Close()
	}

	// Delete temporary directory that contains unix socket files
	dirPath := filepath.Dir(plugo.Ln.Addr().String())
	os.Remove(dirPath)

	os.Exit(0)
}

func Shutdown() {
	termc <- syscall.SIGINT
}

// Codec functions
func encode(values ...any) []byte {
	if len(values) == 0 {
		return []byte{}
	}

	encodedValues := make([]byte, 0, 1024)

	for _, value := range values {
		// Determine the type of value
		valueType := reflect.TypeOf(value)

		var valueBytes []byte

		// Convert value to bytes based on type
		switch valueType.Kind() {
		case reflect.Bool:
			valueBytes = make([]byte, 1)

			if value.(bool) {
				valueBytes[0] = 1
			} else {
				valueBytes[0] = 0
			}
		case reflect.Int:
			valueBytes = make([]byte, valueType.Size())

			for i := 0; i < len(valueBytes); i++ {
				valueBytes[i] = byte(value.(int) >> (i * 8))
			}
		case reflect.Float64:
			valueBytes = make([]byte, valueType.Size())

			floatValue := math.Float64bits(value.(float64))
			for i := 0; i < len(valueBytes); i++ {
				valueBytes[i] = byte(floatValue >> (i * 8))
			}
		case reflect.String:
			valueBytes = []byte(value.(string))
		default:
		}

		valueTypeBytes := []byte(valueType.Kind().String())

		encodedValues = append(encodedValues, valueBytes...)
		encodedValues = append(encodedValues, delimFour...)
		encodedValues = append(encodedValues, valueTypeBytes...)
		encodedValues = append(encodedValues, delimThree...)
	}

	// Remove last delimThree as it is not necessary
	encodedValues = encodedValues[:len(encodedValues)-9]

	return encodedValues
}

func decode(encodedValues []byte) []any {
	if len(encodedValues) == 0 {
		return []any{}
	}

	// Split bytes into an array of value-type pairs
	vtPairs := bytes.Split(encodedValues, delimThree)

	// For each value-type pair, retrieve the initial value
	decodedValues := make([]any, len(vtPairs))

	for i, vtPair := range vtPairs {
		// Split each value-type pair into an array with the value and type bytes separated
		valueType := bytes.Split(vtPair, delimFour)

		if len(valueType) != 2 {
			continue
		}

		valueBytes := valueType[0] // Value bytes
		typeBytes := valueType[1]  // Type bytes

		switch string(typeBytes) {
		case reflect.Bool.String():
			if valueBytes[0] == 1 {
				decodedValues[i] = true
			} else {
				decodedValues[i] = false
			}
		case reflect.Int.String():
			value := 0

			for j, b := range valueBytes {
				value += int(b) << (j * 8)
			}

			decodedValues[i] = value
		case reflect.Float64.String():
			var value uint64 = 0

			for j, b := range valueBytes {
				value += uint64(b) << (j * 8)
			}

			decodedValues[i] = math.Float64frombits(value)
		case reflect.String.String():
			decodedValues[i] = string(valueBytes)
		default:
		}
	}

	return decodedValues
}

// Pings the parent once every ms milliseconds, if no response is received
func (plugo *Plugo) ShutdownIfParentDies(ms int64) {
    go func() {
        for {
            resp, err := plugo.Call(plugo.ParentId, "__alive__")

            if err != nil {
	            termc <- syscall.SIGINT
            }

            if resp[0].(bool) != true {
	            termc <- syscall.SIGINT
            }

            time.Sleep(ms*time.Millisecond)
        }
    }()
}

// Types

type Plugo struct {
	Id       string
	ParentId string
	Ln       *net.UnixListener
	Exposed  map[string]FuncSig
	Conns    map[string]*net.UnixConn
}

type FunctionCall struct {
	funcName string
	args     []any
}

type FuncSig func(...any) []any
