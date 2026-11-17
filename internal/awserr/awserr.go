package awserr

import (
	"fmt"
	"net/http"
)

// Fault says which side of the call caused an error.
type Fault int

const (
	// FaultSender marks an error caused by the caller's request.
	FaultSender Fault = iota + 1
	// FaultReceiver marks an error caused by the service.
	FaultReceiver
)

// Error is an AWS service error. The json protocol writes Code as the
// "__type" member; the query protocol writes it as the Error Code element.
type Error struct {
	// Code is the wire code, for example "QueueDoesNotExist".
	Code string
	// Message is the human-readable description.
	Message string
	// StatusCode is the HTTP status of the response.
	StatusCode int
	// Fault is the side that caused the error.
	Fault Fault
	// QueryCode is the awsQueryCompatible code, for example
	// "AWS.SimpleQueueService.NonExistentQueue". It is empty when there is none.
	QueryCode string
}

// Error returns the code and message as "<Code>: <Message>".
func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// NotImplemented returns the 501 error for an operation the service does not
// implement yet.
func NotImplemented(service, operation string) *Error {
	return &Error{
		Code:       "InternalFailure",
		Message:    fmt.Sprintf("API action '%s' for service '%s' not yet implemented", operation, service),
		StatusCode: http.StatusNotImplemented,
		Fault:      FaultReceiver,
	}
}

// Internal returns the 500 error for a failure inside the service.
func Internal(msg string) *Error {
	return &Error{
		Code:       "InternalError",
		Message:    msg,
		StatusCode: http.StatusInternalServerError,
		Fault:      FaultReceiver,
	}
}
