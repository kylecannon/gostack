package sqs

import (
	"net/http"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

// Messages that LocalStack words the same way for every caller.
const (
	messageQueueName = "Can only include alphanumeric characters, hyphens, or underscores. 1 to 80 in length"
	messageFIFOName  = "The name of a FIFO queue can only include alphanumeric characters, hyphens, " +
		"or underscores, must end with .fifo suffix and be 1 to 80 in length"
)

// invalidParameterValue is LocalStack's InvalidParameterValueException. The
// generated package has no constructor for it because botocore's spec does not
// declare the shape.
func invalidParameterValue(message string) *awserr.Error {
	return &awserr.Error{
		Code:       "InvalidParameterValueException",
		Message:    message,
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
		QueryCode:  "InvalidParameterValue",
	}
}

// missingParameter is LocalStack's MissingRequiredParameterException.
func missingParameter(name string) *awserr.Error {
	return &awserr.Error{
		Code:       "MissingRequiredParameterException",
		Message:    "The request must contain the parameter " + name + ".",
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
		QueryCode:  "MissingParameter",
	}
}

// queueDoesNotExist is QueueDoesNotExist, worded as the caller's protocol words
// it.
func queueDoesNotExist(rc *request.Context) *awserr.Error {
	if rc.Protocol == protocol.KindQuery {
		return sqsapi.NewQueueDoesNotExist("The specified queue does not exist for this wsdl version.")
	}

	return sqsapi.NewQueueDoesNotExist("The specified queue does not exist.")
}

// purgeInProgress is PurgeQueueInProgress, which LocalStack answers with 403
// where the generated constructor says 400.
func purgeInProgress(queueName string) *awserr.Error {
	err := sqsapi.NewPurgeQueueInProgress("Only one PurgeQueue operation on " + queueName +
		" is allowed every 60 seconds.")
	err.StatusCode = http.StatusForbidden

	return err
}
