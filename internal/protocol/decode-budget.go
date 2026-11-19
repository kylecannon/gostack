package protocol

// DecodeSlack is the constant in the memory rule for a request body: what
// decoding may hold beyond DecodeFactor times the body's bytes, whatever the
// body.
//
// The rule: decoding a body of n bytes holds at most DecodeBudget(n) bytes of
// decoded values. With the body itself and the one copy a protocol keeps of it,
// one request holds about four times its body plus a constant. A body that
// would decode into more is refused with the protocol's answer to a body over
// MaxBody, before it is decoded where the protocol can tell in advance (json,
// query) and as soon as it passes the budget where it decodes as it reads
// (rest-xml).
//
// 16 MiB lets every request the specs allow through whatever its shape, and
// the densest request a test pins: an SQS SendMessageBatch of 65,536 entries
// sent as a form, 131,075 pairs that SQS must answer with the count it was
// sent (TooManyEntriesInBatchRequest), as LocalStack and AWS do.
const DecodeSlack = 16 << 20

// DecodeFactor is how many bytes of decoded values each byte of a body may
// cost, for a service that does not declare its own
// (service.Registration.DecodeFactor). The legal requests of S3, SQS and STS
// stay far below it; docs/architecture.md lists their worst ratios.
const DecodeFactor = 2

// DecodeBudget is the most bytes decoding a body of n bytes may hold, at
// DecodeFactor: twice the body, plus DecodeSlack.
func DecodeBudget(n int) int64 {
	return DecodeBudgetAt(n, DecodeFactor)
}

// DecodeBudgetAt is DecodeBudget for a service that declares its own factor:
// factor times the body, plus DecodeSlack. A factor of zero or less is
// DecodeFactor.
func DecodeBudgetAt(n, factor int) int64 {
	if factor <= 0 {
		factor = DecodeFactor
	}

	return int64(factor)*int64(n) + DecodeSlack
}
