package sdk

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"unicode/utf8"
)

func readRecord(r *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(frame)+len(part) > MaxRecord+2 {
			return nil, fmt.Errorf("%w: record too large", ErrProtocol)
		}
		if err == bufio.ErrBufferFull {
			frame = append(frame, part...)
			continue
		}
		if err != nil {
			if err == io.EOF && len(frame)+len(part) != 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if frame == nil {
			frame = part
		} else {
			frame = append(frame, part...)
		}
		frame = bytes.TrimSuffix(bytes.TrimSuffix(frame, []byte("\n")), []byte("\r"))
		if len(frame) > MaxRecord {
			return nil, fmt.Errorf("%w: record too large", ErrProtocol)
		}
		return frame, nil
	}
}

// encoding/json replaces invalid Unicode and accepts repeated keys by default; the wire does not.
func parseJSON(data []byte) (any, error) {
	if !utf8.Valid(data) || !escapedUTF16(data) {
		return nil, ErrProtocol
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := readValue(d, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrProtocol
	}
	return v, nil
}

func escapedUTF16(data []byte) bool {
	for i := 0; i+1 < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		i++
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		code, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code < 0xdc00 {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			tail, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || tail < 0xdc00 || tail > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

func readValue(d *json.Decoder, depth int) (any, error) {
	if depth > 256 {
		return nil, fmt.Errorf("JSON nesting limit")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		out := make(map[string]any)
		for d.More() {
			t, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := t.(string)
			if !ok {
				return nil, ErrProtocol
			}
			if _, found := out[key]; found {
				return nil, fmt.Errorf("duplicate object key")
			}
			v, err := readValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
		_, err := d.Token()
		return out, err
	case json.Delim('['):
		out := make([]any, 0)
		for d.More() {
			v, err := readValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		_, err := d.Token()
		return out, err
	case json.Delim('}'), json.Delim(']'):
		return nil, ErrProtocol
	default:
		if n, ok := t.(json.Number); ok {
			f, err := n.Float64()
			if err != nil || math.IsInf(f, 0) {
				return nil, fmt.Errorf("nonfinite number")
			}
			// Wire numbers have JavaScript semantics; bound exponent and decimal work in validation.
			if math.Abs(f) <= maxSafe && f == math.Trunc(f) {
				return json.Number(strconv.FormatInt(int64(f), 10)), nil
			}
			return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), nil
		}
		return t, nil
	}
}

func decode[T any](v any) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return out, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	return out, nil
}

func keys(v any, required ...string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			return false
		}
	}
	return true
}

func asUint(v any) (uint64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	u, err := strconv.ParseUint(string(n), 10, 64)
	return u, err == nil && u <= maxSafe
}
