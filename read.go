package whatsapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/Rhymen/go-whatsapp/binary"
	"github.com/Rhymen/go-whatsapp/crypto/cbc"
)

func (wac *Conn) readPump() {
	defer func() {
		wac.wg.Done()
		_, _ = wac.Disconnect()
	}()

	var readErr error
	var msgType int
	var reader io.Reader

	for {
		readerFound := make(chan struct{})
		go func() {
			if wac.ws != nil {
				msgType, reader, readErr = wac.ws.conn.NextReader()
			}
			close(readerFound)
		}()
		select {
		case <-readerFound:
			if readErr != nil {
				wac.handle(&ErrConnectionFailed{Err: readErr})
				return
			}
			msg, err := ioutil.ReadAll(reader)
			if err != nil {
				wac.handle(fmt.Errorf("error reading message from Reader: %w", err))
				continue
			}
			err = wac.processReadData(msgType, msg)
			if err != nil {
				wac.handle(fmt.Errorf("error processing data: %w", err))
			}
		case <-wac.ws.close:
			return
		}
	}
}

func (wac *Conn) processReadData(msgType int, msg []byte) error {
	data := strings.SplitN(string(msg), ",", 2)

	if data[0][0] == '!' { //Keep-Alive Timestamp
		data = append(data, data[0][1:]) //data[1]
		data[0] = "!"
	}

	if len(data) == 2 && len(data[1]) == 0 {
		return nil
	}

	if len(data) != 2 || len(data[1]) == 0 {
		return ErrInvalidWsData
	}

	wac.listener.RLock()
	listener, hasListener := wac.listener.m[data[0]]
	wac.listener.RUnlock()

	if hasListener {
		// listener only exists for TextMessages query messages out of contact.go
		// If these binary query messages can be handled another way,
		// then the TextMessages, which are all JSON encoded, can directly
		// be unmarshalled. The listener chan could then be changed from type
		// chan string to something like chan map[string]interface{}. The unmarshalling
		// in several places, especially in session.go, would then be gone.
		listener <- data[1]
		close(listener)
		wac.removeListener(data[0])
	} else if msgType == websocket.BinaryMessage {
		wac.loginSessionLock.RLock()
		sess := wac.session
		wac.loginSessionLock.RUnlock()
		if sess == nil || sess.MacKey == nil || sess.EncKey == nil {
			return ErrInvalidWsState
		}
		message, err := wac.decryptBinaryMessage([]byte(data[1]))
		if err != nil {
			return fmt.Errorf("error decoding binary: %w", err)
		}
		wac.dispatch(message)
	} else { //RAW json status updates
		wac.handle(string(data[1]))
	}
	return nil
}

func (wac *Conn) decryptBinaryMessage(msg []byte) (*binary.Node, error) {
	//message validation
	h2 := hmac.New(sha256.New, wac.session.MacKey)
	if len(msg) < 33 {
		var response struct {
			Status int `json:"status"`
		}

		if err := json.Unmarshal(msg, &response); err == nil {
			if response.Status == http.StatusNotFound {
				return nil, ErrServerRespondedWith404
			}
			return nil, fmt.Errorf("server responded with %d", response.Status)
		} else {
			return nil, ErrInvalidServerResponse
		}

		return nil, ErrInvalidServerResponse

	}
	h2.Write([]byte(msg[32:]))
	if !hmac.Equal(h2.Sum(nil), msg[:32]) {
		return nil, ErrInvalidHmac
	}

	// message decrypt
	d, err := cbc.Decrypt(wac.session.EncKey, nil, msg[32:])
	if err != nil {
		return nil, fmt.Errorf("decrypting message with AES-CBC failed: %w", err)
	}

	// message unmarshal
	message, err := binary.Unmarshal(d)
	if err != nil {
		return nil, fmt.Errorf("could not decode binary: %w", err)
	}

	return message, nil
}
