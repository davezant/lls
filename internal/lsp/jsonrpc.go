// Package lsp implements the Language Server Protocol transport and
// request handlers on top of the engine package.
//
// The transport is JSON-RPC 2.0 framed with "Content-Length" headers over
// stdin/stdout. Requests are processed strictly in order on one goroutine:
// the engine answers in microseconds, so parallelism would only add locking.
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// message is any JSON-RPC message (request, notification or response).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSON-RPC / LSP error codes.
const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeRequestFailed  = -32803
)

// conn reads and writes framed JSON-RPC messages.
type conn struct {
	r   *bufio.Reader
	w   *bufio.Writer
	buf []byte // reusable read buffer
	out []byte // reusable write buffer
}

func newConn(r io.Reader, w io.Writer) *conn {
	return &conn{r: bufio.NewReaderSize(r, 64<<10), w: bufio.NewWriterSize(w, 64<<10)}
}

// read returns the next message body (valid until the next call).
func (c *conn) read() ([]byte, error) {
	length := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if length >= 0 {
				break
			}
			continue // tolerate stray blank lines
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("lsp: bad Content-Length %q", v)
			}
			length = n
		}
	}
	if cap(c.buf) < length {
		c.buf = make([]byte, length)
	}
	c.buf = c.buf[:length]
	if _, err := io.ReadFull(c.r, c.buf); err != nil {
		return nil, err
	}
	return c.buf, nil
}

// write sends one message body and flushes.
func (c *conn) write(body []byte) error {
	c.out = c.out[:0]
	c.out = append(c.out, "Content-Length: "...)
	c.out = strconv.AppendInt(c.out, int64(len(body)), 10)
	c.out = append(c.out, "\r\n\r\n"...)
	if _, err := c.w.Write(c.out); err != nil {
		return err
	}
	if _, err := c.w.Write(body); err != nil {
		return err
	}
	return c.w.Flush()
}

// reply sends a successful response whose result is already JSON encoded.
func (c *conn) reply(id json.RawMessage, result []byte) error {
	b := make([]byte, 0, len(result)+48)
	b = append(b, `{"jsonrpc":"2.0","id":`...)
	b = append(b, id...)
	b = append(b, `,"result":`...)
	b = append(b, result...)
	b = append(b, '}')
	return c.write(b)
}

// replyError sends an error response.
func (c *conn) replyError(id json.RawMessage, code int, msg string) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b := []byte(`{"jsonrpc":"2.0","id":`)
	b = append(b, id...)
	b = append(b, `,"error":{"code":`...)
	b = strconv.AppendInt(b, int64(code), 10)
	b = append(b, `,"message":`...)
	b = appendString(b, msg)
	b = append(b, "}}"...)
	return c.write(b)
}

// notify sends a notification with JSON-encodable params.
func (c *conn) notify(method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	b := []byte(`{"jsonrpc":"2.0","method":`)
	b = appendString(b, method)
	b = append(b, `,"params":`...)
	b = append(b, p...)
	b = append(b, '}')
	return c.write(b)
}

// appendString appends s as a JSON string literal. It is a hand-written
// encoder because completion responses contain hundreds of strings and
// encoding/json's reflection is the dominant cost otherwise.
func appendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c < utf8.RuneSelf {
			i++
			continue
		}
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				b = append(b, s[start:i]...)
				b = append(b, `�`...)
				i++
				start = i
				continue
			}
			if r == ' ' || r == ' ' {
				b = append(b, s[start:i]...)
				b = append(b, `\u202`...)
				b = append(b, hex[r&0xF])
				i += size
				start = i
				continue
			}
			i += size
			continue
		}
		b = append(b, s[start:i]...)
		switch c {
		case '"':
			b = append(b, `\"`...)
		case '\\':
			b = append(b, `\\`...)
		case '\n':
			b = append(b, `\n`...)
		case '\r':
			b = append(b, `\r`...)
		case '\t':
			b = append(b, `\t`...)
		default:
			b = append(b, `\u00`...)
			b = append(b, hex[c>>4], hex[c&0xF])
		}
		i++
		start = i
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}
