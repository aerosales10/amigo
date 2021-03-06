package amigo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aerosales10/amigo/uuid"
)

const (
	pingActionID       = "AmigoPing"
	amigoConnIDKey     = "AmigoConnID"
	commandResponseKey = "CommandResponse"
)

var (
	actionTimeout = 3 * time.Second
	dialTimeout   = 10 * time.Second
	pingInterval  = 5 * time.Second
	sequence      uint64
)

type amiAdapter struct {
	id         string
	eventsChan chan map[string]string

	dialString string
	username   string
	password   string

	connected     bool
	actionTimeout time.Duration
	dialTimeout   time.Duration

	actionsChan    chan map[string]string
	responseChans  map[string]chan map[string]string
	pingerChan     chan struct{}
	disconnectChan chan bool
	mutex          *sync.RWMutex
	emitEvent      func(string, string)
}

func newAMIAdapter(s *Settings, eventEmitter func(string, string)) *amiAdapter {
	var a = new(amiAdapter)
	a.dialString = fmt.Sprintf("%s:%s", s.Host, s.Port)
	a.username = s.Username
	a.password = s.Password
	a.actionTimeout = s.ActionTimeout
	a.dialTimeout = s.DialTimeout
	a.mutex = &sync.RWMutex{}
	a.emitEvent = eventEmitter

	a.actionsChan = make(chan map[string]string)
	a.responseChans = make(map[string]chan map[string]string)
	a.eventsChan = make(chan map[string]string, 4096)
	a.pingerChan = make(chan struct{})
	a.disconnectChan = make(chan bool)

	return a
}

func (a *amiAdapter) disconnect() {
	a.disconnectChan <- true
	close(a.disconnectChan)
}

func (a *amiAdapter) connect(reconnectInterval time.Duration, keepalive bool) {
	var conn net.Conn
BREAKLOOP:
	for {
		readErrChan := make(chan error)
		writeErrChan := make(chan error)
		pingErrChan := make(chan error)
		chanStop := make(chan struct{})
		select {
		case err := <-readErrChan:
			close(chanStop)
			a.mutex.Lock()
			a.connected = false
			a.mutex.Unlock()
			go a.emitEvent("error", fmt.Sprintf("AMI TCP ERROR: %s", err.Error()))
		case err := <-writeErrChan:
			close(chanStop)
			a.mutex.Lock()
			a.connected = false
			a.mutex.Unlock()
			go a.emitEvent("error", fmt.Sprintf("AMI TCP ERROR: %s", err.Error()))
		case err := <-pingErrChan:
			close(chanStop)
			a.mutex.Lock()
			a.connected = false
			a.mutex.Unlock()
			go a.emitEvent("error", fmt.Sprintf("AMI TCP ERROR: %s", err.Error()))
		case <-a.disconnectChan:
			break BREAKLOOP
		default:
			a.mutex.RLock()
			connected := a.connected
			a.mutex.RUnlock()
			if connected {
				time.Sleep(time.Second * 1)
				continue BREAKLOOP
			}
			c, err := a.openConnection()
			if err == nil {
				greetings := make([]byte, 100)
				n, err := c.Read(greetings)
				if err != nil {
					go a.emitEvent("error", fmt.Sprintf("Asterisk connection error: %s", err.Error()))
					time.Sleep(reconnectInterval)
					continue BREAKLOOP
				}

				err = a.login(c)
				if err != nil {
					go a.emitEvent("error", fmt.Sprintf("Asterisk login error: %s", err.Error()))
					time.Sleep(reconnectInterval)
					continue BREAKLOOP
				}

				if n > 2 {
					greetings = greetings[:n-2]
				}
				a.emitEvent("connect", string(greetings))
				a.mutex.Lock()
				a.connected = true
				a.mutex.Unlock()
				if conn != nil {
					conn.Close()
				}
				conn = c
				defer conn.Close()
				go a.reader(conn, chanStop, readErrChan)
				go a.writer(conn, chanStop, writeErrChan)
				if keepalive {
					go a.pinger(chanStop, pingErrChan)
				}
			} else {
				a.emitEvent("error", "AMI Reconnect failed")
				time.Sleep(reconnectInterval)
				continue BREAKLOOP
			}
		}
	}

}

func nextID() string {
	i := atomic.AddUint64(&sequence, 1)
	return strconv.Itoa(int(i))
}

func (a *amiAdapter) pinger(stop <-chan struct{}, errChan chan error) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	ping := map[string]string{"Action": "Ping", "ActionID": pingActionID, amigoConnIDKey: a.id}
	for {
		select {
		case <-stop:
			return
		default:
		}

		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		if !a.online() {
			// when stop chan didn't received before ticker
			return
		}

		a.actionsChan <- ping
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-a.pingerChan:
			timer.Stop()
			continue
		case <-timer.C:
			errChan <- errors.New("ping timeout")
			return
		}
	}
}

func (a *amiAdapter) writer(conn net.Conn, stop <-chan struct{}, writeErrChan chan error) {
	for {
		select {
		case <-stop:
			return
		default:
		}

		select {
		case <-stop:
			return
		case action := <-a.actionsChan:
			if action[amigoConnIDKey] != a.id {
				// action sent before reconnect, need to be ignored
				continue
			}
			data := serialize(action)
			_, err := conn.Write(data)
			if err != nil {
				writeErrChan <- err
				return
			}
		}
	}
}

func (a *amiAdapter) distribute(event map[string]string) {
	actionID := event["ActionID"]
	if actionID == pingActionID {
		a.pingerChan <- struct{}{}
		return
	}

	if len(a.eventsChan) == cap(a.eventsChan) {
		a.emitEvent("error", "events chan is full")
	}

	// TODO: Need to decide to send or not to send action responses to eventsChan
	a.eventsChan <- event
	if len(actionID) > 0 {
		a.mutex.RLock()
		resChan := a.responseChans[actionID]
		a.mutex.RUnlock()
		if resChan != nil {
			a.mutex.Lock()
			resChan = a.responseChans[actionID]
			if resChan == nil {
				a.mutex.Unlock()
				return
			}

			delete(a.responseChans, actionID)
			a.mutex.Unlock()
			resChan <- event
		}
	}
}

func (a *amiAdapter) exec(action map[string]string) map[string]string {
	action[amigoConnIDKey] = a.id
	actionID := action["ActionID"]
	if actionID == "" {
		actionID = uuid.NewV4()
		action["ActionID"] = actionID
	}

	// TODO: parse multi-message response
	resChan := make(chan map[string]string)
	a.mutex.Lock()
	a.responseChans[actionID] = resChan
	a.mutex.Unlock()

	a.actionsChan <- action

	time.AfterFunc(a.actionTimeout, func() {
		a.mutex.RLock()
		_, ok := a.responseChans[actionID]
		a.mutex.RUnlock()
		if ok {
			a.mutex.Lock()
			if ch, ok := a.responseChans[actionID]; ok {
				delete(a.responseChans, actionID)
				a.mutex.Unlock()
				ch <- map[string]string{"Error": "Timeout"}
				return
			}
			a.mutex.Unlock()
		}
	})

	response := <-resChan

	return response
}

func (a *amiAdapter) login(conn net.Conn) error {
	var action = map[string]string{
		"Action":   "Login",
		"Username": a.username,
		"Secret":   a.password,
	}

	serialized := serialize(action)
	_, err := conn.Write(serialized)
	if err != nil {
		return err
	}

	reader := bufio.NewReader(conn)
	result, err := readMessage(reader)
	if err != nil {
		return err
	}

	if result["Response"] != "Success" && result["Message"] != "Authentication accepted" {
		return errors.New(result["Message"])
	}

	return nil
}

func (a *amiAdapter) online() bool {
	a.mutex.RLock()
	defer a.mutex.RUnlock()
	return a.connected
}

func (a *amiAdapter) openConnection() (net.Conn, error) {
	return net.DialTimeout("tcp", a.dialString, a.dialTimeout)
}

func readMessage(r *bufio.Reader) (m map[string]string, err error) {
	m = make(map[string]string)
	var responseFollows bool
	for {
		kv, _, err := r.ReadLine()
		if len(kv) == 0 {
			return m, err
		}

		var key string
		i := bytes.IndexByte(kv, ':')
		if i >= 0 {
			endKey := i
			for endKey > 0 && kv[endKey-1] == ' ' {
				endKey--
			}
			key = string(kv[:endKey])
		}

		if key == "" && !responseFollows {
			if err != nil {
				return m, err
			}

			continue
		}

		if responseFollows && key != "Privilege" && key != "ActionID" {
			if string(kv) != "--END COMMAND--" {
				if len(m[commandResponseKey]) == 0 {
					m[commandResponseKey] = string(kv)
				} else {
					m[commandResponseKey] = fmt.Sprintf("%s\n%s", m[commandResponseKey], string(kv))
				}
			}

			if err != nil {
				return m, err
			}

			continue
		}

		i++
		for i < len(kv) && (kv[i] == ' ' || kv[i] == '\t') {
			i++
		}
		value := string(kv[i:])

		if key == "Response" && value == "Follows" {
			responseFollows = true
		}

		m[key] = value

		if err != nil {
			return m, err
		}
	}
}

func serialize(data map[string]string) []byte {
	var outBuf bytes.Buffer

	for key := range data {
		outBuf.WriteString(key)
		outBuf.WriteString(": ")
		outBuf.WriteString(data[key])
		outBuf.WriteString("\r\n")
	}
	outBuf.WriteString("\r\n")

	return outBuf.Bytes()
}

func (a *amiAdapter) reader(conn net.Conn, stop <-chan struct{}, readErrChan chan error) {
	chanErr := make(chan error)
	chanEvents := make(chan map[string]string)
	go func() {
		bufReader := bufio.NewReader(conn)
		for i := 0; ; i++ {
			var event map[string]string
			var err error
			event, err = readMessage(bufReader)
			if err != nil {
				chanErr <- err
				return
			}

			event["#"] = strconv.Itoa(i)
			event["TimeReceived"] = time.Now().Format(time.RFC3339Nano)
			chanEvents <- event
		}
	}()

	for {
		select {
		case <-stop:
			return
		default:
		}

		select {
		case <-stop:
			return
		case err := <-chanErr:
			readErrChan <- err
		case event := <-chanEvents:
			a.distribute(event)
		}
	}
}
