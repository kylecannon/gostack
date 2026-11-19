package ssm

import (
	"context"
	"strconv"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
)

// PutParameter creates a parameter or, with Overwrite, adds a version to one.
// LocalStack cleans the name without checking it (provider.py:135-146); the
// checks are moto's (models.py put_parameter), in its order.
func (p *Provider) PutParameter(ctx context.Context, in *ssmapi.PutParameterInput) (*ssmapi.PutParameterOutput, error) {
	c, err := p.callOf(ctx)
	if err != nil {
		return nil, err
	}

	if in.Name == nil {
		return nil, missing("name")
	}

	name := normalizeName(*in.Name)

	err = checkPut(name, in)
	if err != nil {
		return nil, err
	}

	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()

	v, err := s.put(name, in, p.now())
	if err != nil {
		return nil, err
	}

	return &ssmapi.PutParameterOutput{Version: new(v.Version), Tier: new(v.Tier)}, nil
}

// checkPut makes the checks of a put that need no state.
func checkPut(name string, in *ssmapi.PutParameterInput) error {
	if deref(in.Value) == "" {
		return ssmapi.NewValidationException("1 validation error detected: Value '' at 'value' failed to satisfy " +
			"constraint: Member must have length greater than or equal to 1.")
	}

	if isTrue(in.Overwrite) && len(in.Tags) > 0 {
		return ssmapi.NewValidationException("Invalid request: tags and overwrite can't be used together. To create a " +
			"parameter with tags, please remove overwrite flag. To update tags for an existing parameter, please use " +
			"AddTagsToResource or RemoveTagsFromResource.")
	}

	return reservedNameError(name)
}

// put stores the version a put makes and returns it. The caller holds the lock
// for writing.
func (s *State) put(name string, in *ssmapi.PutParameterInput, now timeNow) (*version, error) {
	key := storeKey(name)
	existing := s.params[key]
	overwrite := isTrue(in.Overwrite)

	if existing == nil && !overwrite {
		switch t := deref(in.Type); {
		case t == "":
			return nil, ssmapi.NewValidationException("A parameter type is required when you create a parameter.")
		case !validType(t):
			return nil, ssmapi.NewValidationException("1 validation error detected: Value '" + t + "' at 'type' failed " +
				"to satisfy constraint: Member must satisfy enum value set: [SecureString, StringList, String]")
		}
	}

	dataType := dataTypeText
	if in.DataType != nil {
		dataType = *in.DataType
	}

	if dataType != dataTypeText && dataType != dataTypeImage {
		return nil, ssmapi.NewValidationException("The following data type is not supported: " + dataType +
			" (Data type names are all lowercase.)")
	}

	v := &version{
		Name: name, Type: deref(in.Type), Description: in.Description, AllowedPattern: in.AllowedPattern,
		KeyID: in.KeyId, Modified: now, Version: 1, DataType: dataType, Tier: deref(in.Tier), Policies: in.Policies,
	}

	if existing != nil {
		err := existing.inherit(v, in)
		if err != nil {
			return nil, err
		}
	}

	v.seal(deref(in.Value))

	if existing == nil {
		existing = &parameter{}
		s.add(key, existing)
	}

	existing.versions = append(existing.versions, v)

	if len(in.Tags) > 0 {
		existing.tags = mergeTags(existing.tags, in.Tags)
	}

	return v, nil
}

// inherit makes v the next version of p: what the request leaves out comes
// from the current version. It fails when p cannot take another version.
func (p *parameter) inherit(v *version, in *ssmapi.PutParameterInput) error {
	if !isTrue(in.Overwrite) {
		return ssmapi.NewParameterAlreadyExists("The parameter already exists. To overwrite this value, set the " +
			"overwrite option in the request to true.")
	}

	prev := p.latest()
	v.Version = prev.Version + 1

	if v.Type == "" {
		v.Type = prev.Type
	}

	if len(p.versions) >= versionLimit {
		oldest := p.versions[0]
		if len(oldest.Labels) > 0 {
			return ssmapi.NewParameterMaxVersionLimitExceeded("You attempted to create a new version of " + v.Name +
				" by calling the PutParameter API with the overwrite flag. Version " +
				strconv.FormatInt(oldest.Version, 10) + ", the oldest version, can't be deleted because it has a " +
				"label associated with it. Move the label to another version of the parameter, and try again.")
		}

		p.versions = p.versions[1:]
	}

	v.Description = cmpOr(v.Description, prev.Description)
	v.AllowedPattern = cmpOr(v.AllowedPattern, prev.AllowedPattern)
	v.KeyID = cmpOr(v.KeyID, prev.KeyID)
	v.Policies = cmpOr(v.Policies, prev.Policies)

	if in.DataType == nil {
		v.DataType = prev.DataType
	}

	if in.Tier == nil {
		v.Tier = prev.Tier
	}

	return nil
}

// seal sets the stored value and the defaults moto fills in on a new version.
func (v *version) seal(value string) {
	if v.Tier == "" {
		v.Tier = tierStandard
	}

	if v.Type != typeSecureString {
		v.Value = value

		return
	}

	if v.KeyID == nil || *v.KeyID == "" {
		v.KeyID = new(defaultKeyID)
	}

	v.Value = "kms:" + *v.KeyID + ":" + value
}

// validType reports whether t is a parameter type.
func validType(t string) bool {
	return t == typeString || t == typeStringList || t == typeSecureString
}

// cmpOr returns a when it is set, and b otherwise.
func cmpOr[T any](a, b *T) *T {
	if a != nil {
		return a
	}

	return b
}

// mergeTags adds tags to into, a new map when into is nil, and returns it.
func mergeTags(into map[string]string, tags []ssmapi.Tag) map[string]string {
	if into == nil {
		into = make(map[string]string, len(tags))
	}

	for _, tag := range tags {
		into[deref(tag.Key)] = deref(tag.Value)
	}

	return into
}
