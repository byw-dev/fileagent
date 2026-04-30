package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// requestIDKey is the header name for the request-id correlation field.
const requestIDHeader = "X-Request-ID"

// RequestID injects a unique request-id into every response. If the client
// already provides an X-Request-ID header the value is reused; otherwise a new
// UUID v4 is generated. The request-id is also stored in the Gin context as
// "request_id" for use in error responses and log fields.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if id == "" {
			id = uuid.New().String()
		}
		c.Set("request_id", id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

// ErrorResponse is the standard envelope returned on any API error.
// system-design.md §5.11 defines this structure.
type ErrorResponse struct {
	Error     ErrorDetail `json:"error"`
	RequestID string      `json:"request_id"`
}

// ErrorDetail holds the machine-readable error code and human-readable message.
type ErrorDetail struct {
	Code    string      `json:"code"`
	Message string      `json:"message"`
	Detail  interface{} `json:"detail,omitempty"`
}

// AbortWithError is a helper that terminates the Gin handler chain and writes
// a standard error response. Use this instead of c.AbortWithStatusJSON so
// that error format stays consistent across all handlers.
func AbortWithError(c *gin.Context, httpStatus int, code, message string, detail interface{}) {
	reqID, _ := c.Get("request_id")
	c.AbortWithStatusJSON(httpStatus, ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
			Detail:  detail,
		},
		RequestID: asString(reqID),
	})
}

// NotImplemented is a convenience handler that returns 501 with a standard
// error body. Assign it directly to routes whose business logic is not yet
// implemented (Phase 1 placeholder).
func NotImplemented(c *gin.Context) {
	AbortWithError(c, http.StatusNotImplemented, "NOT_IMPLEMENTED",
		"This endpoint is not yet implemented", nil)
}

func asString(v interface{}) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}
