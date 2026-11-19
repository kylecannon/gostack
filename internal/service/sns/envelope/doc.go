// Package envelope builds the JSON body SNS delivers to a subscriber for a
// Notification, SubscriptionConfirmation or UnsubscribeConfirmation, signs it
// with SignatureVersion 1 (SHA1withRSA) or 2 (SHA256withRSA), and serves the
// certificate that verifies the signature at CertPath.
//
// Field order follows AWS: the SQS bodies in LocalStack's aws.validated
// snapshots for notifications, and the AWS HTTP/HTTPS confirmation document
// for the confirmations. Byte layout and string escaping follow LocalStack's
// json.dumps. MessageAttributes are written sorted by name; no snapshot depends
// on another order, since the snapshot comparison parses the JSON. Sign signs
// a message once for every subscriber of its topic, and Signed.Body writes it
// for one. Raw message delivery and per-protocol message selection are the
// caller's: an envelope wraps the message text it is given.
package envelope
