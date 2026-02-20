// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package codec

import (
	"io"
	"reflect"

	"github.com/fxamacker/cbor/v2"
)

// encMode is the CBOR encoder configured with Core Deterministic
// Encoding (RFC 8949 §4.2): sorted map keys, smallest integer
// encoding, no indefinite-length items. Same logical data always
// produces identical bytes.
var encMode cbor.EncMode

// decMode is the CBOR decoder configured to accept standard CBOR.
// Unknown fields are silently ignored for forward compatibility.
var decMode cbor.DecMode

func init() {
	var err error

	encOptions := cbor.CoreDetEncOptions()
	// Types implementing encoding.TextMarshaler (ref.Entity, ref.Fleet,
	// ref.RoomID, etc.) serialize as CBOR text strings via MarshalText.
	// Without this, struct fields with unexported data (like ref.Entity)
	// would serialize as empty CBOR maps, losing their identity.
	encOptions.TextMarshaler = cbor.TextMarshalerTextString
	encMode, err = encOptions.EncMode()
	if err != nil {
		panic("codec: CBOR encoder initialization failed: " + err.Error())
	}

	decMode, err = cbor.DecOptions{
		// Bureau never uses non-string map keys. When the decoder's
		// target is interface{}/any (e.g., map[string]any values), it
		// must pick a concrete Go map type. The CBOR default is
		// map[interface{}]interface{} (since CBOR allows non-string
		// keys), but that type is incompatible with encoding/json and
		// most Go code that expects map[string]any. This setting only
		// affects any-typed targets — struct field decoding is
		// unaffected.
		DefaultMapType: reflect.TypeOf(map[string]any(nil)),
		// Types implementing encoding.TextUnmarshaler (ref.Entity,
		// ref.Fleet, ref.RoomID, etc.) deserialize from CBOR text
		// strings via UnmarshalText. Mirrors the TextMarshaler setting
		// above for round-trip correctness.
		TextUnmarshaler: cbor.TextUnmarshalerTextString,
	}.DecMode()
	if err != nil {
		panic("codec: CBOR decoder initialization failed: " + err.Error())
	}
}

// Marshal encodes v to CBOR using Core Deterministic Encoding.
func Marshal(v any) ([]byte, error) {
	return encMode.Marshal(v)
}

// Unmarshal decodes CBOR data into v.
func Unmarshal(data []byte, v any) error {
	return decMode.Unmarshal(data, v)
}

// Encoder is a CBOR stream encoder. Type alias so consumers import
// only lib/codec, not fxamacker/cbor directly.
type Encoder = cbor.Encoder

// Decoder is a CBOR stream decoder. Type alias so consumers import
// only lib/codec, not fxamacker/cbor directly.
type Decoder = cbor.Decoder

// RawMessage is a raw encoded CBOR value. It implements
// cbor.Marshaler and cbor.Unmarshaler so it can be used to delay
// CBOR decoding or pre-encode CBOR output.
type RawMessage = cbor.RawMessage

// NewEncoder returns a CBOR encoder that writes to w using Bureau's
// standard Core Deterministic Encoding configuration.
func NewEncoder(w io.Writer) *Encoder {
	return encMode.NewEncoder(w)
}

// NewDecoder returns a CBOR decoder that reads from r using Bureau's
// standard decoding configuration.
func NewDecoder(r io.Reader) *Decoder {
	return decMode.NewDecoder(r)
}

// Diagnose returns the CBOR diagnostic notation (RFC 8949 §8) for the
// entire contents of data.
func Diagnose(data []byte) (string, error) {
	return cbor.Diagnose(data)
}

// DiagnoseFirst returns the CBOR diagnostic notation for the first
// data item in data, along with the remaining unconsumed bytes. Use
// this to process CBOR sequences one item at a time.
func DiagnoseFirst(data []byte) (string, []byte, error) {
	return cbor.DiagnoseFirst(data)
}
