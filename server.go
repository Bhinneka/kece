package kece

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// Server struct
type Server struct {
	clients       map[*Client]bool
	args          *Arguments
	register      chan *Client
	unregister    chan *Client
	publish       chan []byte
	clientMessage chan *ClientMessage
	commander     Commander
	done          chan bool
	sync.RWMutex
}

// NewServer function, Server's constructor
func NewServer(args *Arguments, commander Commander) *Server {
	clients := make(map[*Client]bool)
	register := make(chan *Client)
	unregister := make(chan *Client)
	publish := make(chan []byte)
	clientMessage := make(chan *ClientMessage)
	done := make(chan bool, 1)
	return &Server{
		args:          args,
		clients:       clients,
		register:      register,
		unregister:    unregister,
		publish:       publish,
		clientMessage: clientMessage,
		commander:     commander,
		done:          done,
	}
}

//addClient function will push new client to the map clients
func (server *Server) addClient(key *Client, b bool) {
	server.Lock()
	printYellowColor(fmt.Sprintf("log -> new client connected %s\n", key.ID))
	server.clients[key] = b
	server.Unlock()
}

//deleteClient function will delete client by specific key from map clients
func (server *Server) deleteClient(key *Client) {
	server.Lock()
	delete(server.clients, key)
	server.Unlock()
}

func (server *Server) serveClient() {

	for {
		select {
		case client := <-server.register:
			// register client to client collection
			server.addClient(client, true)

			// handle message from client
			go func() {
				defer func() {
					err := client.Conn.Close()
					if err != nil {
						log.Printf("Error when closing the client. Err: %v", err)
					}
					server.unregister <- client
				}()

				for {
					message, err := bufio.NewReader(client.Conn).ReadBytes('\n')
					if err != nil {
						server.unregister <- client
						break
					}

					server.clientMessage <- &ClientMessage{Client: client, Message: message}
				}
			}()
		case client := <-server.unregister:
			if _, ok := server.clients[client]; ok {
				printRedColor(fmt.Sprintf("client %s unregister its connection\n", client.ID))
				server.deleteClient(client)
			}
		case clientMessage := <-server.clientMessage:
			printCyanColor(fmt.Sprintf("Received message : %s from %s\n", string(clientMessage.Message), clientMessage.Client.ID))

			go processMessage(clientMessage, server.commander, server.args.Auth)
		}
	}

}

// Start function, start Kece server
func (server *Server) Start() error {
	listener, err := net.Listen(server.args.Network, fmt.Sprintf(":%s", server.args.Port))
	if err != nil {
		return err
	}

	printGreenColor(Banner)
	printYellowColor(fmt.Sprintf("log -> kece server listen on port : %s\n", server.args.Port))

	defer func() {
		err := listener.Close()
		if err != nil {
			log.Printf("Failed to close listener. Err: %v", err)
		}
	}()

	kill := make(chan os.Signal, 1)

	// notify when user interrupt the process
	signal.Notify(kill, syscall.SIGINT, syscall.SIGTERM)

	// handle concurrent client
	go server.serveClient()

	go server.waitOSNotify(kill)

	// handle concurrent incoming client
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				fmt.Println("server stopped")
				return
			}

			//register to every connected client to DB
			server.register <- &Client{ID: c.RemoteAddr().String(), Conn: c}
		}
	}()

	<-server.done

	return nil

}

func (server *Server) waitOSNotify(kill chan os.Signal) {
	for {
		select {
		case <-kill:
			fmt.Println("server daemon interrupted")
			server.done <- true
			return
		}
	}
}

func validateAuth(cm *ClientMessage, commander Commander, auth string) error {

	clientID := cm.Client.ID

	result, err := commander.Get([]byte(commands["GET"]), []byte(clientID))
	if err != nil {
		return errors.New(ErrorInvalidAuth)
	}

	if !bytes.Equal([]byte(auth), result.Value) {
		return errors.New(ErrorInvalidAuth)
	}

	return nil
}

func writeMessage(cm *ClientMessage, message []byte) {
	_, err := cm.Client.Conn.Write(message)
	if err != nil {
		log.Printf("Failed to write response. Err: %v", err)
	}
}

func processExpired(ctx context.Context, cm *ClientMessage, commander Commander) {
	c, cancel := context.WithTimeout(ctx, cm.Exp)
	defer cancel()

	select {
	case <-c.Done():
		err := commander.Delete([]byte("DEL"), cm.Key)
		if err != nil {
			log.Printf("Failed to delete value. Err: %v", err)
		}
	default:
	}
}

func processMessage(cm *ClientMessage, commander Commander, auth string) {
	for {
		if err := cm.ValidateMessage(); err != nil {
			writeMessage(cm, []byte(err.Error()))
			return
		}

		cmd := cm.Cmd
		key := cm.Key

		switch string(cmd) {
		case commands["AUTH"]:
			value := cm.Key
			value = bytes.Trim(value, crlf)
			if len(auth) <= 0 {
				reply := replies["ERROR"]
				writeMessage(cm, []byte(reply))
				return
			}

			if !bytes.Equal([]byte(auth), value) {
				writeMessage(cm, []byte(ErrorInvalidAuth))
				return
			}

			key = []byte(cm.Client.ID)

			err := commander.Auth(cmd, key, value)
			if err != nil {
				reply := replies["ERROR"]
				writeMessage(cm, []byte(reply))
				return
			}

			reply := replies["OK"]
			writeMessage(cm, []byte(reply))
			return
		case commands["SET"]:
			if len(auth) > 0 {
				if err := validateAuth(cm, commander, auth); err != nil {
					writeMessage(cm, []byte(err.Error()))
					return
				}
			}

			value := cm.Value
			_, err := commander.Set(cmd, key, value)
			if err != nil {
				reply := replies["ERROR"]
				writeMessage(cm, []byte(reply))
				return
			}

			if cm.Exp != 0 {
				go processExpired(context.Background(), cm, commander)
			}

			reply := replies["OK"]
			writeMessage(cm, []byte(reply))
			return
		case commands["GET"]:
			if len(auth) > 0 {
				if err := validateAuth(cm, commander, auth); err != nil {
					writeMessage(cm, []byte(err.Error()))
					return
				}
			}

			result, err := commander.Get(cmd, key)
			if err != nil {
				reply := replies["ERROR"]
				writeMessage(cm, []byte(reply))
				return
			}

			reply := result.Value
			writeMessage(cm, reply)
			writeMessage(cm, []byte(crlf))
			return
		case commands["DEL"]:
			if len(auth) > 0 {
				if err := validateAuth(cm, commander, auth); err != nil {
					writeMessage(cm, []byte(err.Error()))
					return
				}
			}

			err := commander.Delete(cmd, key)
			if err != nil {
				reply := replies["ERROR"]
				writeMessage(cm, []byte(reply))
				return
			}

			reply := replies["OK"]
			writeMessage(cm, []byte(reply))
			return
		default:
			writeMessage(cm, []byte(ErrorInvalidCommand))
			return
		}
	}
}
