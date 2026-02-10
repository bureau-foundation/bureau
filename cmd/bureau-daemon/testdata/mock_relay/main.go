// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// mock_relay is a minimal observation relay binary used in daemon tests.
// It speaks the observation protocol on fd 3 (inherited from the daemon):
//   - Sends a metadata message with session name from argv[1]
//   - Sends an empty history message
//   - Echoes data messages back, prefixed with "echo:" to verify round-trip
//
// The binary exits when fd 3 closes or when it receives a read error.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
)

const (
	messageTypeData     byte = 0x01
	messageTypeHistory  byte = 0x03
	messageTypeMetadata byte = 0x04
	headerLength             = 5
)

func writeMessage(writer io.Writer, messageType byte, payload []byte) error {
	var header [headerLength]byte
	header[0] = messageType
	binary.BigEndian.PutUint32(header[1:5], uint32(len(payload)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := writer.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func readMessage(reader io.Reader) (byte, []byte, error) {
	var header [headerLength]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	messageType := header[0]
	payloadLength := binary.BigEndian.Uint32(header[1:5])
	payload := make([]byte, payloadLength)
	if payloadLength > 0 {
		if _, err := io.ReadFull(reader, payload); err != nil {
			return 0, nil, err
		}
	}
	return messageType, payload, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mock-relay <session-name>")
		os.Exit(1)
	}
	sessionName := os.Args[1]

	socketFile := os.NewFile(uintptr(3), "daemon-socket")
	if socketFile == nil {
		fmt.Fprintln(os.Stderr, "fd 3 not available")
		os.Exit(1)
	}
	connection, err := net.FileConn(socketFile)
	socketFile.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FileConn: %v\n", err)
		os.Exit(1)
	}
	defer connection.Close()

	// Send metadata message.
	metadata := map[string]any{
		"session":   sessionName,
		"principal": "@test/echo:bureau.local",
		"machine":   "@machine/test:bureau.local",
		"panes":     []any{},
	}
	metadataJSON, _ := json.Marshal(metadata)
	if err := writeMessage(connection, messageTypeMetadata, metadataJSON); err != nil {
		fmt.Fprintf(os.Stderr, "write metadata: %v\n", err)
		os.Exit(1)
	}

	// Send empty history message.
	if err := writeMessage(connection, messageTypeHistory, nil); err != nil {
		fmt.Fprintf(os.Stderr, "write history: %v\n", err)
		os.Exit(1)
	}

	// Echo loop: read data messages, write them back prefixed with "echo:".
	for {
		messageType, payload, readErr := readMessage(connection)
		if readErr != nil {
			return
		}
		if messageType == messageTypeData && len(payload) > 0 {
			echoPayload := append([]byte("echo:"), payload...)
			if writeErr := writeMessage(connection, messageTypeData, echoPayload); writeErr != nil {
				return
			}
		}
	}
}
