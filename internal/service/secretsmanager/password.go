package secretsmanager

import (
	"context"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/awserr"
)

const (
	defaultPasswordLength = 32
	minPasswordLength     = 4
	maxPasswordLength     = 4096
)

// GetRandomPassword is moto's random_password: characters drawn from the
// classes not excluded, the last ones replaced by one of each class when
// RequireEachIncludedType holds, which it does by default.
func (*Provider) GetRandomPassword(
	_ context.Context, in *api.GetRandomPasswordInput,
) (*api.GetRandomPasswordOutput, error) {
	length, lerr := passwordLength(in.PasswordLength)
	if lerr != nil {
		return nil, lerr
	}

	exclude := deref(in.ExcludeCharacters)
	flag := func(b *bool) bool { return b != nil && *b }

	var alphabet, required strings.Builder

	addClass := func(class string) {
		alphabet.WriteString(class)
		addRequired(&required, class, exclude)
	}

	switch {
	case !flag(in.ExcludeLowercase) && !flag(in.ExcludeUppercase):
		alphabet.WriteString(letters)
		addRequired(&required, lowercase, exclude)
		addRequired(&required, uppercase, exclude)
	case !flag(in.ExcludeLowercase):
		addClass(lowercase)
	case !flag(in.ExcludeUppercase):
		addClass(uppercase)
	default:
		// Both cases excluded: no letters.
	}

	if !flag(in.ExcludeNumbers) {
		addClass(digits)
	}

	if !flag(in.ExcludePunctuation) {
		addClass(punctuation)
	}

	if flag(in.IncludeSpace) {
		alphabet.WriteByte(' ')
		required.WriteByte(' ')
	}

	chars := without(alphabet.String(), exclude)
	if chars == "" {
		return nil, api.NewInvalidParameterException("Password length is too short based on the required types.")
	}

	password := randomString(chars, int(length))

	if in.RequireEachIncludedType == nil || *in.RequireEachIncludedType {
		keep := max(len(password)-required.Len(), 0)
		password = password[:keep] + required.String()
	}

	return &api.GetRandomPasswordOutput{RandomPassword: new(password)}, nil
}

// passwordLength is PasswordLength, 32 by default, within moto's limits.
func passwordLength(given *int64) (int64, *awserr.Error) {
	length := int64(defaultPasswordLength)
	if given != nil {
		length = *given
	}

	if length > maxPasswordLength {
		return 0, errValidation("1 validation error detected: Value '" + strconv.FormatInt(length, 10) +
			"' at 'passwordLength' failed to satisfy constraint: Member must have value less than or equal to 4096")
	}

	if length < minPasswordLength {
		return 0, api.NewInvalidParameterException("Password length is too short based on the required types.")
	}

	return length, nil
}

// addRequired adds one character of class that exclude leaves. moto fails when
// ExcludeCharacters empties a class; here the class adds none.
func addRequired(required *strings.Builder, class, exclude string) {
	if left := without(class, exclude); left != "" {
		required.WriteByte(left[randomIndex(len(left))])
	}
}

// without returns s without the characters of exclude.
func without(s, exclude string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(exclude, r) {
			return -1
		}

		return r
	}, s)
}
