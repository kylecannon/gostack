package s3

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/request"
)

const (
	// destinationMessage is what a notification target that does not check out
	// is answered with. The target's own reason is the argument value.
	destinationMessage = "Unable to validate the following destination configurations"

	arnFieldCount = 6

	sqsService      = "sqs"
	sqsTargetPrefix = "AmazonSQS."

	// snsService is the signing name of SNS. Its query calls carry the version of
	// its API; a Publish carries the subject S3 gives every notification.
	snsService     = "sns"
	snsVersion     = "2010-03-31"
	snsSubject     = "Amazon S3 Notification"
	snsContentType = "application/x-www-form-urlencoded"
	snsTopicArn    = "TopicArn"

	// queueHost is the host of the queue URLs notifications are sent to. SQS
	// takes the region from its first label and the account and the name from the
	// path, so one host serves every queue.
	queueHost   = "localhost.localstack.cloud:4566"
	queueScheme = "http"
)

// A target is a kind of notification destination: the service its ARN names and
// what an error calls the ARN member.
type target struct {
	service  string
	argument string
	pattern  *regexp.Regexp
}

func newTarget(service, argument string) target {
	return target{
		service: service, argument: argument,
		pattern: regexp.MustCompile("^arn:(aws|aws-cn|aws-eusc|aws-iso|aws-iso-b|aws-us-gov):" + service + ":"),
	}
}

var (
	queueTarget  = newTarget(sqsService, "Queue")
	topicTarget  = newTarget(snsService, snsTopicArn)
	lambdaTarget = newTarget("lambda", "LambdaFunctionArn")
)

// GetBucketNotificationConfiguration returns the bucket's notification
// configuration, empty for a bucket that has none.
func (p *Provider) GetBucketNotificationConfiguration(
	ctx context.Context, in *s3api.GetBucketNotificationConfigurationRequest,
) (*s3api.GetBucketNotificationConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &s3api.GetBucketNotificationConfigurationOutput{}

	err = b.readConfig(func() error {
		if b.Notification != nil {
			*out = *b.Notification
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutBucketNotificationConfiguration replaces the notification configuration.
// Every configuration is checked first: its ARN, its filter rules and, unless
// SkipDestinationValidation says not to, that its target exists and takes a test
// message. Nothing is stored when one fails. A queue and a topic are the targets
// gostack checks, through SQS and SNS; a Lambda configuration is kept and read
// back, and no event is delivered to it.
func (p *Provider) PutBucketNotificationConfiguration(
	ctx context.Context, in *s3api.PutBucketNotificationConfigurationInput,
) (*s3api.PutBucketNotificationConfigurationOutput, error) {
	b, err := p.configBucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	cfg := in.NotificationConfiguration
	if cfg == nil {
		cfg = &s3api.GetBucketNotificationConfigurationOutput{}
	}

	// A queue and a topic are verified unless the request says not to.
	var verify verifiers

	if in.SkipDestinationValidation == nil || !*in.SkipDestinationValidation {
		verify = verifiers{
			queue: func(arn string) error { return p.verifyQueue(ctx, b, arn) },
			topic: func(arn string) error { return p.verifyTopic(ctx, b, arn) },
		}
	}

	err = checkNotifications(cfg, verify)
	if err != nil {
		return nil, err
	}

	err = b.writeConfig(func() error {
		b.Notification = cfg

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &s3api.PutBucketNotificationConfigurationOutput{}, nil
}

// verifiers check that the target of a queue or a topic ARN exists and takes a
// test message. A nil one leaves that kind of target unchecked.
type verifiers struct {
	queue func(arn string) error
	topic func(arn string) error
}

// checkNotifications validates the configurations in the order LocalStack does:
// topics, queues, then Lambda functions, and in each list as given. It gives
// an id to a configuration that has none and spells its filter rule names with a
// capital.
func checkNotifications(cfg *s3api.GetBucketNotificationConfigurationOutput, verify verifiers) error {
	for i := range cfg.TopicConfigurations {
		topic := &cfg.TopicConfigurations[i]

		err := checkConfiguration(topicTarget, &topic.Id, deref(topic.TopicArn), topic.Filter, verify.topic)
		if err != nil {
			return err
		}
	}

	for i := range cfg.QueueConfigurations {
		queue := &cfg.QueueConfigurations[i]

		err := checkConfiguration(queueTarget, &queue.Id, deref(queue.QueueArn), queue.Filter, verify.queue)
		if err != nil {
			return err
		}
	}

	for i := range cfg.LambdaFunctionConfigurations {
		fn := &cfg.LambdaFunctionConfigurations[i]

		err := checkConfiguration(lambdaTarget, &fn.Id, deref(fn.LambdaFunctionArn), fn.Filter, nil)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkConfiguration is _validate_notification. verify, when it is not nil,
// checks that the target of the ARN exists.
func checkConfiguration(
	t target, id **string, arn string, filter *s3api.NotificationConfigurationFilter, verify func(arn string) error,
) error {
	if deref(*id) == "" {
		*id = new(shortID())
	}

	if !t.pattern.MatchString(arn) {
		return invalidArgument("The ARN could not be parsed", t.argument, &arn)
	}

	if verify != nil {
		err := verify(arn)
		if err != nil {
			return err
		}
	}

	if filter == nil || filter.Key == nil {
		return nil
	}

	for i := range filter.Key.FilterRules {
		rule := &filter.Key.FilterRules[i]
		if rule.Name == nil || rule.Value == nil {
			return malformedXMLError()
		}

		switch strings.ToLower(*rule.Name) {
		case "prefix", "suffix":
			rule.Name = new(strings.ToUpper((*rule.Name)[:1]) + strings.ToLower((*rule.Name)[1:]))
		default:
			return invalidArgument("filter rule name must be either prefix or suffix", "FilterRule.Name", rule.Name)
		}
	}

	return nil
}

// verifyQueue asks SQS whether the queue exists and sends it S3's test event,
// both through the gateway as the bucket's owner. A gateway that does not serve
// SQS, or no gateway, leaves the target unchecked, as LocalStack's is when its
// SQS is not enabled.
func (p *Provider) verifyQueue(ctx context.Context, b *Bucket, arn string) error {
	q := parseARN(arn)
	if q.err != nil {
		return invalidArgument("The ARN could not be parsed", queueTarget.argument, &arn)
	}

	lookup, err := encodeJSON(map[string]string{"QueueName": q.resource, "QueueOwnerAWSAccountId": q.account})
	if err != nil {
		return notifyInternal(err)
	}

	resp, err := p.client.Do(ctx, inproc.Call{
		Account: q.account, Region: q.region, Service: sqsService, Target: sqsTargetPrefix + "GetQueueUrl", Body: lookup,
	})

	switch {
	case errors.Is(err, inproc.ErrNotBound), err == nil && resp.NotEnabled():
		return nil
	case err != nil:
		return notifyInternal(err)
	case !resp.OK():
		return invalidArgument(destinationMessage, arn, new("The destination queue does not exist"))
	}

	var found struct {
		QueueURL string `json:"QueueUrl"`
	}

	err = json.Unmarshal(resp.Body, &found)
	if err != nil || found.QueueURL == "" {
		return invalidArgument(destinationMessage, arn, new("The destination queue does not exist"))
	}

	return p.sendTestEvent(ctx, b, q, found.QueueURL, arn)
}

// sendTestEvent is the s3:TestEvent message S3 sends a queue when it is
// configured.
func (p *Provider) sendTestEvent(ctx context.Context, b *Bucket, q arnParts, queueURL, arn string) error {
	payload, err := testEvent(ctx, b)
	if err != nil {
		return notifyInternal(err)
	}

	body, err := encodeJSON(sendMessageInput{QueueURL: queueURL, MessageBody: string(payload)})
	if err != nil {
		return notifyInternal(err)
	}

	resp, err := p.client.Do(ctx, inproc.Call{
		Account: b.Account, Region: q.region, Service: sqsService, Target: sqsTargetPrefix + "SendMessage", Body: body,
	})
	if err != nil || !resp.OK() {
		return invalidArgument(destinationMessage, arn,
			new("Permissions on the destination queue do not allow S3 to publish notifications from this bucket"))
	}

	return nil
}

// testEvent is the s3:TestEvent document S3 sends a target when it is configured.
func testEvent(ctx context.Context, b *Bucket) ([]byte, error) {
	var requestID string
	if rc, ok := request.From(ctx); ok {
		requestID = rc.RequestID
	}

	return encodeJSON(map[string]any{
		"Service": "Amazon S3", "Event": "s3:TestEvent", "Time": time.Now().UTC().Format(eventTimeFormat),
		"Bucket": b.Name, "RequestId": requestID, "HostId": recordHostID,
	})
}

// verifyTopic asks SNS whether the topic exists, as the owner of its ARN, and
// publishes S3's test event to it as the bucket's owner, both through the
// gateway. A gateway that does not serve SNS, or no gateway, leaves the target
// unchecked, as LocalStack's is when its SNS is not enabled.
func (p *Provider) verifyTopic(ctx context.Context, b *Bucket, arn string) error {
	t := parseARN(arn)
	if t.err != nil {
		return invalidArgument("The ARN could not be parsed", topicTarget.argument, &arn)
	}

	resp, err := p.client.Do(ctx, topicCall(t, t.account, "GetTopicAttributes", url.Values{snsTopicArn: {arn}}))

	switch {
	case errors.Is(err, inproc.ErrNotBound), err == nil && resp.NotEnabled():
		return nil
	case err != nil:
		return notifyInternal(err)
	case !resp.OK():
		return invalidArgument(destinationMessage, arn, new("The destination topic does not exist"))
	}

	payload, err := testEvent(ctx, b)
	if err != nil {
		return notifyInternal(err)
	}

	resp, err = p.client.Do(ctx, publishCall(t, b.Account, arn, string(payload)))
	if err != nil || !resp.OK() {
		return invalidArgument(destinationMessage, arn,
			new("Permissions on the destination topic do not allow S3 to publish notifications from this bucket"))
	}

	return nil
}

// topicCall is the query call of an SNS action to the region the topic's ARN
// names, as account. params go in the form next to the action.
func topicCall(t arnParts, account, action string, params url.Values) inproc.Call {
	form := url.Values{"Action": {action}, "Version": {snsVersion}}
	maps.Copy(form, params)

	return inproc.Call{
		Account: account, Region: t.region, Service: snsService, ContentType: snsContentType,
		Body: []byte(form.Encode()),
	}
}

// publishCall is the call that publishes body to the topic arn as account, with
// the subject S3 gives every notification.
func publishCall(t arnParts, account, arn, body string) inproc.Call {
	return topicCall(t, account, "Publish", url.Values{
		snsTopicArn: {arn}, "Message": {body}, "Subject": {snsSubject},
	})
}

// notifyInternal is the answer to a validation that could not be made at all.
func notifyInternal(err error) error {
	return configError("InternalError", "We encountered an internal error. Please try again. "+err.Error(),
		http.StatusInternalServerError)
}

// arnParts is an ARN split the way botocore's ArnParser does.
type arnParts struct {
	region   string
	account  string
	resource string
	err      error
}

var errARN = errors.New("invalid ARN")

// parseARN splits the six fields of an ARN; the resource keeps its colons.
func parseARN(arn string) arnParts {
	fields := strings.SplitN(arn, ":", arnFieldCount)
	if len(fields) < arnFieldCount {
		return arnParts{err: errARN}
	}

	return arnParts{region: fields[3], account: fields[4], resource: fields[5]}
}

// sendMessageInput is the body of an SQS SendMessage over the JSON protocol.
type sendMessageInput struct {
	QueueURL                string                      `json:"QueueUrl"`
	MessageBody             string                      `json:"MessageBody"`
	MessageSystemAttributes map[string]messageAttribute `json:"MessageSystemAttributes,omitempty"`
}

type messageAttribute struct {
	DataType    string `json:"DataType"`
	StringValue string `json:"StringValue"`
}

// sendMessageCall is the call that puts body on the queue q, as the account that
// owns the bucket. The X-Ray header of the request that caused it goes on as the
// message's trace header.
func sendMessageCall(q arnParts, account, body, xray string) (inproc.Call, error) {
	queueURL := url.URL{
		Scheme: queueScheme, Host: "sqs." + q.region + "." + queueHost, Path: "/" + q.account + "/" + q.resource,
	}
	in := sendMessageInput{QueueURL: queueURL.String(), MessageBody: body}

	if xray != "" {
		in.MessageSystemAttributes = map[string]messageAttribute{
			sqsSystemAttributeTrace: {DataType: "String", StringValue: xray},
		}
	}

	encoded, err := encodeJSON(in)
	if err != nil {
		return inproc.Call{}, err
	}

	return inproc.Call{
		Account: account, Region: q.region, Service: sqsService, Target: sqsTargetPrefix + "SendMessage", Body: encoded,
	}, nil
}
