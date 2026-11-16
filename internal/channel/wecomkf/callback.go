package wecomkf

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

var (
	ErrSign    = errors.New("invalid callback signature")
	ErrDecrypt = errors.New("invalid callback payload")
	ErrCorp    = errors.New("invalid callback recipient")
)

type Callback struct {
	corp, token string
	block       cipher.Block
	iv          []byte
}

type Event struct {
	XMLName xml.Name `xml:"xml"`
	To      string   `xml:"ToUserName"`
	Kind    string   `xml:"MsgType"`
	Name    string   `xml:"Event"`
	Token   string   `xml:"Token"`
	Account string   `xml:"OpenKfId"`
}

func NewCallback(corp, token, encodingKey string) (*Callback, error) {
	key, err := base64.RawStdEncoding.DecodeString(encodingKey)
	if err != nil || len(encodingKey) != 43 || len(key) != 32 {
		return nil, errors.New("WECOM_AES_KEY must be a 43-character EncodingAESKey")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return &Callback{corp: corp, token: token, block: block, iv: key[:aes.BlockSize]}, nil
}

func (c *Callback) Verify(q url.Values) ([]byte, error) {
	return c.decrypt(q, q.Get("echostr"))
}

func (c *Callback) Receive(q url.Values, raw []byte) (Event, error) {
	var envelope struct {
		XMLName xml.Name `xml:"xml"`
		Encrypt string   `xml:"Encrypt"`
	}
	if err := xml.Unmarshal(raw, &envelope); err != nil || envelope.Encrypt == "" {
		return Event{}, ErrDecrypt
	}
	body, err := c.decrypt(q, envelope.Encrypt)
	if err != nil {
		return Event{}, err
	}
	var ev Event
	if err := xml.Unmarshal(body, &ev); err != nil {
		return Event{}, ErrDecrypt
	}
	if ev.To != c.corp {
		return Event{}, ErrCorp
	}
	if ev.Kind == "event" && ev.Name == "kf_msg_or_event" && (ev.Token == "" || ev.Account == "") {
		return Event{}, fmt.Errorf("%w: missing notification fields", ErrDecrypt)
	}
	return ev, nil
}

func (c *Callback) decrypt(q url.Values, encoded string) ([]byte, error) {
	timestamp, nonce, signature := q.Get("timestamp"), q.Get("nonce"), q.Get("msg_signature")
	if timestamp == "" || nonce == "" || signature == "" || encoded == "" {
		return nil, ErrSign
	}
	fields := []string{c.token, timestamp, nonce, encoded}
	slices.Sort(fields)
	expected := sha1.Sum([]byte(strings.Join(fields, "")))
	provided, err := hex.DecodeString(signature)
	if err != nil || subtle.ConstantTimeCompare(provided, expected[:]) != 1 {
		return nil, ErrSign
	}
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(body) == 0 || len(body)%32 != 0 {
		return nil, ErrDecrypt
	}
	cipher.NewCBCDecrypter(c.block, c.iv).CryptBlocks(body, body)
	padding := int(body[len(body)-1])
	if padding < 1 || padding > 32 {
		return nil, ErrDecrypt
	}
	for _, b := range body[len(body)-padding:] {
		if int(b) != padding {
			return nil, ErrDecrypt
		}
	}
	body = body[:len(body)-padding]
	if len(body) < 20 {
		return nil, ErrDecrypt
	}
	length := uint64(binary.BigEndian.Uint32(body[16:20]))
	if length > uint64(len(body)-20) {
		return nil, ErrDecrypt
	}
	end := 20 + int(length)
	if string(body[end:]) != c.corp {
		return nil, ErrCorp
	}
	return body[20:end], nil
}
