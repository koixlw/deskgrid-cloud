package internal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// HMAC window: requests with timestamp older than this are rejected.
// Prevents replay attacks where a sniffed request gets replayed later.
const hmacMaxSkew = 5 * time.Minute

// SignRequest computes the X-Signature header for a body. The signature is
// hex(HMAC-SHA256(secret, timestamp + ":" + body)).
func SignRequest(secret []byte, body []byte, ts int64) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(strconv.FormatInt(ts, 10)))
	h.Write([]byte{':'})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyRequest checks signature and timestamp. Returns nil if valid.
func VerifyRequest(secret []byte, body []byte, signature string, ts int64, now time.Time) error {
	// Reject obvious replays / future-dated timestamps
	if ts <= 0 {
		return errBadRequest("missing or invalid X-Timestamp")
	}
	diff := now.Sub(time.Unix(ts, 0))
	if diff < -hmacMaxSkew || diff > hmacMaxSkew {
		return errBadRequest("timestamp out of window")
	}
	expected := SignRequest(secret, body, ts)
	// constant-time compare
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(signature))) {
		return errUnauthorized("signature mismatch")
	}
	return nil
}

// helper for clearer error messages
type hmacError struct {
	status int
	msg    string
}

func (e *hmacError) Error() string { return e.msg }

func errBadRequest(msg string) error  { return &hmacError{400, msg} }
func errUnauthorized(msg string) error { return &hmacError{401, msg} }

// SignedPost signs the body with HMAC and adds the timestamp/signature
// headers before posting. Used by the gateway when pushing tasks to workers
// so workers can verify the request is from the gateway.
func SignedPost(secret []byte, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, io.NopCloser(strings.NewReader(string(body))))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if len(secret) > 0 {
		ts := time.Now().Unix()
		req.Header.Set("X-Timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("X-Signature", SignRequest(secret, body, ts))
	}
	return workerHTTP.Do(req)
}

// HMACMiddleware enforces request signing on routes that need it. If the
// gateway secret is empty (dev mode), all requests pass.
func HMACMiddleware(secret []byte) gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(secret) == 0 {
			c.Next()
			return
		}
		sig := c.GetHeader("X-Signature")
		tsStr := c.GetHeader("X-Timestamp")
		ts, _ := strconv.ParseInt(tsStr, 10, 64)
		// Read body; cache so handler can read it again
		var body []byte
		if c.Request.Body != nil {
			b, _ := io.ReadAll(c.Request.Body)
			body = b
			c.Request.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		if err := VerifyRequest(secret, body, sig, ts, time.Now()); err != nil {
			e := err.(*hmacError)
			c.AbortWithStatusJSON(e.status, gin.H{"error": e.msg})
			return
		}
		c.Next()
	}
}