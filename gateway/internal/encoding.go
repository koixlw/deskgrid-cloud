// Package internal: tolerant JSON body decoding.
//
// Background: Chinese Windows shells (cmd, bash on Windows) send UTF-8
// bytes that are actually GBK-encoded text. Go's encoding/json silently
// replaces invalid UTF-8 sequences with U+FFFD (the Unicode replacement
// character), which then propagates through to worker tasks. edge-tts and
// any other downstream consumer that needs the actual Chinese chars fails.
//
// This file adds a `bindJSON` helper that:
//  1. validates UTF-8
//  2. if not, decodes GBK -> UTF-8 first
//  3. then unmarshals
//
// Use bindJSON in handlers instead of c.ShouldBindJSON to make the gateway
// encoding-tolerant. The first 5 minutes of debugging we wasted on this
// motivated the helper.
package internal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// bindJSON is a drop-in replacement for c.ShouldBindJSON that handles
// GBK-encoded request bodies transparently. The standard Gin call is
// replaced at each handler site.
func bindJSON(c *gin.Context, obj any) error {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return err
	}
	decoded, err := decodeBody(raw)
	if err != nil {
		return err
	}
	// Restore body so other middleware/handlers can still read it (we have
	// no other readers in this gateway, but defensive).
	c.Request.Body = io.NopCloser(bytes.NewReader(decoded))
	return json.Unmarshal(decoded, obj)
}

// decodeBody returns UTF-8 bytes, falling back to GBK if the input is not
// valid UTF-8. A short slice or one that round-trips through UTF-8 OK is
// returned as-is.
func decodeBody(raw []byte) ([]byte, error) {
	if utf8.Valid(raw) {
		return raw, nil
	}
	// Looks like non-UTF-8 bytes; try GBK (covers cmd.exe / Windows bash).
	// simplifiedchinese.GBK is the superset that includes GB2312.
	gbkDecoder := simplifiedchinese.GBK.NewDecoder()
	decoded, _, err := transform.Bytes(gbkDecoder, raw)
	if err != nil {
		return nil, errors.New("request body is neither valid UTF-8 nor GBK: " + err.Error())
	}
	if !utf8.Valid(decoded) {
		return nil, errors.New("GBK decoding produced invalid UTF-8 (try UTF-8 or GB18030)")
	}
	return decoded, nil
}