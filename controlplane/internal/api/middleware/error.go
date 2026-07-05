package middleware

import (
	"net/http"
	"sort"
	"strings"

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

// AbortWithError terminates the Gin handler chain and writes a standard error
// response. Use this in middleware (which must stop downstream handlers) so the
// error format — including the top-level request_id — stays consistent.
func AbortWithError(c *gin.Context, httpStatus int, code, message string, detail interface{}) {
	c.AbortWithStatusJSON(httpStatus, newErrorResponse(c, code, message, detail))
}

// RespondError writes a standard error response without aborting the handler
// chain. Use this inside handlers (which return immediately after) so every
// error body carries the same envelope, including the top-level request_id
// (system-design.md §5.11).
func RespondError(c *gin.Context, httpStatus int, code, message string, detail interface{}) {
	c.JSON(httpStatus, newErrorResponse(c, code, message, detail))
}

// newErrorResponse builds the standard error envelope, pulling the request_id
// injected by the RequestID middleware from the Gin context.
func newErrorResponse(c *gin.Context, code, message string, detail interface{}) ErrorResponse {
	reqID, _ := c.Get("request_id")
	return ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
			Detail:  detail,
		},
		RequestID: asString(reqID),
	}
}

// RejectUnknownQuery inspects the request's query string and rejects any key
// not present in the allowed set. When an unknown key is found it writes a 400
// error response listing the offending keys and returns false; the caller must
// stop processing. It returns true when every query key is allowed.
//
// This turns silent no-ops (a mistyped filter that used to return 200 with the
// filter ignored) into an explicit client error (06 契约瑕疵 / CC-5).
func RejectUnknownQuery(c *gin.Context, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		set[k] = struct{}{}
	}
	var unknown []string
	for k := range c.Request.URL.Query() {
		if _, ok := set[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return true
	}
	sort.Strings(unknown)
	RespondError(c, http.StatusBadRequest, "INVALID_QUERY_PARAM",
		"unknown query parameter(s): "+strings.Join(unknown, ", "),
		gin.H{"unknown_params": unknown})
	return false
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
