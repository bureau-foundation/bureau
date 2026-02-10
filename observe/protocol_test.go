// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"bytes"
	"testing"
)

func TestWriteReadMessageRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		message Message
	}{
		{
			name:    "data message",
			message: NewDataMessage([]byte("hello terminal")),
		},
		{
			name:    "empty data message",
			message: NewDataMessage(nil),
		},
		{
			name:    "resize message",
			message: NewResizeMessage(120, 40),
		},
		{
			name:    "history message",
			message: NewHistoryMessage([]byte("\x1b[31mred text\x1b[0m\n")),
		},
		{
			name:    "metadata message",
			message: NewMetadataMessage([]byte(`{"session":"bureau/test","principal":"@test:bureau.local"}`)),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var buffer bytes.Buffer
			if err := WriteMessage(&buffer, test.message); err != nil {
				t.Fatalf("WriteMessage: %v", err)
			}

			got, err := ReadMessage(&buffer)
			if err != nil {
				t.Fatalf("ReadMessage: %v", err)
			}

			if got.Type != test.message.Type {
				t.Errorf("type: got 0x%02x, want 0x%02x", got.Type, test.message.Type)
			}
			if !bytes.Equal(got.Payload, test.message.Payload) {
				t.Errorf("payload: got %q, want %q", got.Payload, test.message.Payload)
			}
		})
	}
}

func TestWriteReadMultipleMessages(t *testing.T) {
	t.Parallel()
	var buffer bytes.Buffer

	messages := []Message{
		NewMetadataMessage([]byte(`{"session":"test"}`)),
		NewHistoryMessage([]byte("previous output\n")),
		NewDataMessage([]byte("live data")),
		NewResizeMessage(200, 50),
		NewDataMessage([]byte("more data")),
	}

	for _, message := range messages {
		if err := WriteMessage(&buffer, message); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	for index, want := range messages {
		got, err := ReadMessage(&buffer)
		if err != nil {
			t.Fatalf("ReadMessage[%d]: %v", index, err)
		}
		if got.Type != want.Type {
			t.Errorf("message[%d] type: got 0x%02x, want 0x%02x", index, got.Type, want.Type)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("message[%d] payload: got %q, want %q", index, got.Payload, want.Payload)
		}
	}
}

func TestParseResizePayload(t *testing.T) {
	t.Parallel()
	message := NewResizeMessage(132, 43)
	columns, rows, err := ParseResizePayload(message.Payload)
	if err != nil {
		t.Fatalf("ParseResizePayload: %v", err)
	}
	if columns != 132 {
		t.Errorf("columns: got %d, want 132", columns)
	}
	if rows != 43 {
		t.Errorf("rows: got %d, want 43", rows)
	}
}

func TestParseResizePayloadInvalidLength(t *testing.T) {
	t.Parallel()
	_, _, err := ParseResizePayload([]byte{0x00, 0x01})
	if err == nil {
		t.Fatal("expected error for short payload")
	}
}

func TestReadMessagePayloadTooLarge(t *testing.T) {
	t.Parallel()
	var buffer bytes.Buffer
	// Write a header claiming a payload larger than maxPayloadLength.
	header := []byte{MessageTypeData, 0x01, 0x00, 0x00, 0x01} // 16 MB + 1
	buffer.Write(header)

	_, err := ReadMessage(&buffer)
	if err == nil {
		t.Fatal("expected error for oversized payload")
	}
}
