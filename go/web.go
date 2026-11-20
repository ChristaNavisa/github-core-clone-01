// HTTP wire helpers: the response type every surface returns, JSON with
// the same escaping rules as the reference (no HTML escaping, newline
// terminated), and the git pkt-line / side-band framing.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// HandlerResult is the outcome of one dispatch: a status, headers and a body.
type HandlerResult struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

func marshalJSON(value any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	out := buf.Bytes()
	// json.Encoder appends a newline; keep exactly one.
	if len(out) > 0 && out[len(out)-1] == '\n' {
		return out
	}
	return append(out, '\n')
}

func jsonMarshalString(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(s)
	if err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

func jsonResult(status int, value any) *HandlerResult {
	return &HandlerResult{
		Status:  status,
		Headers: map[string]string{"content-type": "application/json; charset=utf-8"},
		Body:    marshalJSON(value),
	}
}

func emptyResult(status int) *HandlerResult {
	return &HandlerResult{Status: status, Headers: map[string]string{}, Body: []byte{}}
}

func errorResult(status int, message string) *HandlerResult {
	return jsonResult(status, map[string]any{"message": message})
}

// writeTo emits a HandlerResult on an http.ResponseWriter, setting
// Content-Length explicitly so responses are never chunked.
func (r *HandlerResult) writeTo(w http.ResponseWriter) {
	for k, v := range r.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("content-length", strconv.Itoa(len(r.Body)))
	w.WriteHeader(r.Status)
	if len(r.Body) > 0 {
		_, _ = w.Write(r.Body)
	}
}

// ---------------------------------------------------------------------------------------------
// pkt-lines and side-band
// ---------------------------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	length := len(payload) + 4
	out := make([]byte, 0, length)
	out = append(out, []byte(fmt.Sprintf("%04x", length))...)
	out = append(out, payload...)
	return out
}

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }
func pktEnd() []byte   { return []byte("0002") }

type pktLineItem struct {
	Data  []byte
	Flush bool
	Delim bool
	End   bool
	// EndOff is the byte offset just past this packet in the source buffer.
	EndOff int
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// decodePktLines parses a pkt-line stream. Control packets (flush/delim/end)
// are included in the returned items. With stopAtControl, parsing returns as
// soon as a control packet is seen, so the caller can treat the rest of the
// buffer as raw bytes (a packfile). Throws on malformed framing.
func decodePktLines(data []byte, stopAtControl bool) (items []pktLineItem, stopOffset int, err error) {
	off := 0
	for off+4 <= len(data) {
		head := string(data[off : off+4])
		if !isHex4(head) {
			return nil, 0, fmt.Errorf("invalid pkt-line length %q", head)
		}
		length, _ := strconv.ParseInt(head, 16, 32)
		switch length {
		case 0:
			items = append(items, pktLineItem{Flush: true, EndOff: off + 4})
			if stopAtControl {
				return items, off + 4, nil
			}
			off += 4
		case 1:
			items = append(items, pktLineItem{Delim: true, EndOff: off + 4})
			if stopAtControl {
				return items, off + 4, nil
			}
			off += 4
		case 2:
			items = append(items, pktLineItem{End: true, EndOff: off + 4})
			if stopAtControl {
				return items, off + 4, nil
			}
			off += 4
		default:
			if length == 3 || length > 65520 {
				return nil, 0, fmt.Errorf("invalid pkt-line length %d", length)
			}
			if off+int(length) > len(data) {
				return nil, 0, errors.New("truncated pkt-line")
			}
			items = append(items, pktLineItem{Data: data[off+4 : off+int(length)], EndOff: off + int(length)})
			off += int(length)
		}
	}
	if off != len(data) {
		return nil, 0, errors.New("trailing bytes after pkt-lines")
	}
	if stopAtControl {
		return items, off, nil
	}
	return items, len(data), nil
}

// pktLinesText is the text of the data packets, control packets dropped.
func pktLinesText(items []pktLineItem) []string {
	var lines []string
	for _, p := range items {
		if p.Data != nil {
			lines = append(lines, string(p.Data))
		}
	}
	return lines
}

func sidebandPacket(band byte, chunk []byte) []byte {
	payload := append([]byte{band}, chunk...)
	return pktLine(payload)
}

func sidebandChunks(data []byte) []byte {
	var out bytes.Buffer
	const maxChunk = 65515
	if len(data) == 0 {
		out.Write(sidebandPacket(1, []byte{}))
		return out.Bytes()
	}
	for off := 0; off < len(data); {
		end := off + maxChunk
		if end > len(data) {
			end = len(data)
		}
		out.Write(sidebandPacket(1, data[off:end]))
		off = end
	}
	return out.Bytes()
}

func stripLine(s string) string {
	return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
}
