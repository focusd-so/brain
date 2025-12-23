package brain

import (
	"encoding/json"
	"fmt"
)

type ToolError struct {
	Message string
}

// Error implements the error interface.
func (e ToolError) Error() string {
	return e.Message
}

// MarshalJSON implements json.Marshaler to serialize as a string.
func (e ToolError) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.Message)
}

// NewToolError creates a new ToolError with a formatted message.
func NewToolError(format string, args ...any) ToolError {
	return ToolError{Message: fmt.Sprintf(format, args...)}
}

// WrapToolError wraps an existing error as a ToolError.
func WrapToolError(msg string, err error) ToolError {
	if err == nil {
		return ToolError{Message: msg}
	}
	return ToolError{Message: fmt.Sprintf("%s: %v", msg, err.Error())}
}
