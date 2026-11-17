package sts

import (
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/kylecannon/gostack/internal/awserr"
)

const (
	sessionNameAttribute = "https://aws.amazon.com/SAML/Attributes/RoleSessionName"
	nameIDFormatMarker   = "nameid-format:"
)

// samlResponse is the part of a SAML response that AssumeRoleWithSAML reads.
// Names match without a namespace, so samlp:Response and Assertion both parse.
type samlResponse struct {
	Assertion samlAssertion `xml:"Assertion"`
}

type samlAssertion struct {
	Issuer     string          `xml:"Issuer"`
	NameID     samlNameID      `xml:"Subject>NameID"`
	Audience   string          `xml:"Conditions>AudienceRestriction>Audience"`
	Attributes []samlAttribute `xml:"AttributeStatement>Attribute"`
}

type samlNameID struct {
	Format string `xml:"Format,attr"`
	Value  string `xml:",chardata"`
}

type samlAttribute struct {
	Name  string `xml:"Name,attr"`
	Value string `xml:"AttributeValue"`
}

// assertion is what AssumeRoleWithSAML takes from the assertion.
type assertion struct {
	sessionName string
	subject     string
	subjectType string
	issuer      string
	audience    string
}

// parseAssertion decodes a base64 SAML response. The session name is the
// RoleSessionName attribute, or the subject when the attribute is absent.
func parseAssertion(encoded string) (assertion, *awserr.Error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return assertion{}, invalidSAML()
	}

	var parsed samlResponse

	err = xml.Unmarshal(raw, &parsed)
	if err != nil {
		return assertion{}, invalidSAML()
	}

	subject := strings.TrimSpace(parsed.Assertion.NameID.Value)
	found := assertion{
		sessionName: subject,
		subject:     subject,
		issuer:      strings.TrimSpace(parsed.Assertion.Issuer),
		audience:    strings.TrimSpace(parsed.Assertion.Audience),
	}

	if _, kind, ok := strings.Cut(parsed.Assertion.NameID.Format, nameIDFormatMarker); ok {
		found.subjectType = kind
	}

	for _, attribute := range parsed.Assertion.Attributes {
		if attribute.Name == sessionNameAttribute {
			found.sessionName = strings.TrimSpace(attribute.Value)
		}
	}

	if found.sessionName == "" {
		return assertion{}, invalidSAML()
	}

	return found, nil
}

func invalidSAML() *awserr.Error {
	return &awserr.Error{
		Code:       "InvalidIdentityToken",
		Message:    "The SAML assertion could not be parsed.",
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
	}
}
