package sns

import (
	"context"
	"maps"
	"regexp"
	"slices"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/awserr"
)

const (
	attrSMSType       = "DefaultSMSType"
	attrSenderID      = "DefaultSenderID"
	attrMonthlyLimit  = "MonthlySpendLimit"
	defaultSpendLimit = "1"
)

// smsAttributeNames are the attributes SetSMSAttributes takes: LocalStack's
// SMS_ATTRIBUTE_NAMES, and MonthlySpendLimit, which the AWS reference lists and
// LocalStack leaves out.
var smsAttributeNames = []string{
	"DeliveryStatusIAMRole", "DeliveryStatusSuccessSamplingRate", attrSenderID, attrSMSType,
	"UsageReportS3Bucket", attrMonthlyLimit,
}

// smsTypes are LocalStack's SMS_TYPES.
var smsTypes = []string{"Promotional", "Transactional"}

// senderID is LocalStack's SMS_DEFAULT_SENDER_REGEX: 1 to 11 alphanumeric
// characters with at least one letter, as the AWS reference describes
// DefaultSenderID. The lookaheads it uses are written as the checks they make.
var (
	senderChars  = regexp.MustCompile(`^[A-Za-z0-9]{1,11}$`)
	senderLetter = regexp.MustCompile(`[A-Za-z]`)
)

// checkSMSAttributes is LocalStack's _validate_sms_attributes. LocalStack
// reads the sender ID under the name DefaultSendID, which no call can set, so
// it never checks DefaultSenderID; here it is checked as the AWS reference
// describes it. LocalStack reports the first name in request order; a map has
// none, so the first in sorted order is reported.
func checkSMSAttributes(attrs map[string]string) *awserr.Error {
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		if !slices.Contains(smsAttributeNames, name) {
			return api.NewInvalidParameterException(name + " is not a valid attribute")
		}
	}

	if id := attrs[attrSenderID]; id != "" && (!senderChars.MatchString(id) || !senderLetter.MatchString(id)) {
		return api.NewInvalidParameterException(attrSenderID + " is not a valid attribute")
	}

	if kind := attrs[attrSMSType]; kind != "" && !slices.Contains(smsTypes, kind) {
		return api.NewInvalidParameterException(attrSMSType + " is invalid")
	}

	return nil
}

// SetSMSAttributes merges attributes into the caller's SMS attributes of the
// region.
func (p *Provider) SetSMSAttributes(
	ctx context.Context, in *api.SetSMSAttributesInput,
) (*api.SetSMSAttributesOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	err = checkSMSAttributes(in.Attributes)
	if err != nil {
		return nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.Lock()
	maps.Copy(st.smsAttributes, in.Attributes)
	st.mu.Unlock()

	return &api.SetSMSAttributesOutput{}, nil
}

// GetSMSAttributes returns the caller's SMS attributes of the region, those the
// call names or all of them. A region with none set has the default
// MonthlySpendLimit of 1, which LocalStack stores on first use and this only
// reports, so a read changes nothing.
func (p *Provider) GetSMSAttributes(
	ctx context.Context, in *api.GetSMSAttributesInput,
) (*api.GetSMSAttributesOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	st := p.regions.Get(rc.AccountID, rc.Region)

	st.mu.RLock()
	all := maps.Clone(st.smsAttributes)
	st.mu.RUnlock()

	if _, set := all[attrMonthlyLimit]; !set {
		all[attrMonthlyLimit] = defaultSpendLimit
	}

	if len(in.Attributes) > 0 {
		maps.DeleteFunc(all, func(name, _ string) bool { return !slices.Contains(in.Attributes, name) })
	}

	return &api.GetSMSAttributesOutput{Attributes: all}, nil
}

// CheckIfPhoneNumberIsOptedOut reports whether the caller's account has the
// number on its opt-out list. Like LocalStack, it does not check the number.
func (p *Provider) CheckIfPhoneNumberIsOptedOut(
	ctx context.Context, in *api.CheckIfPhoneNumberIsOptedOutInput,
) (*api.CheckIfPhoneNumberIsOptedOutOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	acct := p.accounts.Get(rc.AccountID, accountRegion)

	acct.mu.Lock()
	_, out := acct.optedOut[deref(in.PhoneNumber)]
	acct.mu.Unlock()

	return &api.CheckIfPhoneNumberIsOptedOutOutput{IsOptedOut: &out}, nil
}

// ListPhoneNumbersOptedOut lists the account's opted out numbers in sorted
// order, a page of 100 at a time. LocalStack's NextToken is the first number of
// the next page.
func (p *Provider) ListPhoneNumbersOptedOut(
	ctx context.Context, in *api.ListPhoneNumbersOptedOutInput,
) (*api.ListPhoneNumbersOptedOutOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	acct := p.accounts.Get(rc.AccountID, accountRegion)

	acct.mu.Lock()
	numbers := slices.Sorted(maps.Keys(acct.optedOut))
	acct.mu.Unlock()

	start := 0

	if in.NextToken != nil {
		if i := slices.Index(numbers, *in.NextToken); i >= 0 {
			start = i
		}
	}

	end := min(start+platformsPageSize, len(numbers))
	out := &api.ListPhoneNumbersOptedOutOutput{PhoneNumbers: numbers[start:end]}

	if end < len(numbers) {
		out.NextToken = new(numbers[end])
	}

	return out, nil
}

// OptInPhoneNumber takes a number off the account's opt-out list. A number
// that is not on it is no error.
func (p *Provider) OptInPhoneNumber(
	ctx context.Context, in *api.OptInPhoneNumberInput,
) (*api.OptInPhoneNumberOutput, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	number := deref(in.PhoneNumber)
	if !e164.MatchString(number) {
		return nil, api.NewInvalidParameterException("Invalid parameter: PhoneNumber Reason: input incorrectly formatted")
	}

	acct := p.accounts.Get(rc.AccountID, accountRegion)

	acct.mu.Lock()
	delete(acct.optedOut, number)
	acct.mu.Unlock()

	return &api.OptInPhoneNumberOutput{}, nil
}
