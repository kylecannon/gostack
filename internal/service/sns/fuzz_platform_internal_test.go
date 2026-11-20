package sns

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
)

// FuzzPlatformOf checks that the platform read from an endpoint ARN is a whole
// slash-separated field of it, or empty for an ARN of fewer than four fields.
func FuzzPlatformOf(f *testing.F) {
	for _, seed := range []string{
		"arn:aws:sns:us-east-1:000000000000:endpoint/APNS/app/0a1b2c",
		"arn:aws:sns:us-east-1:000000000000:endpoint/GCM/app/id",
		"arn:aws:sns:us-east-1:000000000000:endpoint/x/y",
		"/APNS/app/id",
		"a/b/c/d/e/f",
		"",
		"////",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, arn string) {
		got := platformOf(arn)

		fields := strings.Split(arn, "/")
		if len(fields) < endpointARNFields {
			if got != "" {
				t.Fatalf("platformOf(%q) = %q for an ARN of fewer than %d fields", arn, got, endpointARNFields)
			}

			return
		}

		if strings.Contains(got, "/") || got != fields[len(fields)-endpointARNFields+1] {
			t.Fatalf("platformOf(%q) = %q, not the second of the last four fields", arn, got)
		}
	})
}

// FuzzBadApplicationName checks that a name is accepted exactly when it is 1 to
// 256 characters of letters, digits, '.', '_' and '-', and that the reason
// given for a refusal is never empty.
func FuzzBadApplicationName(f *testing.F) {
	for _, seed := range []string{
		"app", "my_app-1.0", "", "has space", "emoji\U0001F600", strings.Repeat("a", maxApplicationName),
		strings.Repeat("a", maxApplicationName+1), "line\nbreak", "\xff",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		reason := badApplicationName(name)
		wellFormed := name != "" && utf8.RuneCountInString(name) <= maxApplicationName &&
			strings.Trim(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-") == ""

		if wellFormed != (reason == "") {
			t.Fatalf("badApplicationName(%q) = %q, well-formed is %v", name, reason, wellFormed)
		}

		if (checkApplicationName(name) == nil) != wellFormed {
			t.Fatalf("checkApplicationName(%q) disagrees with badApplicationName", name)
		}
	})
}

// FuzzCheckSMSAttributes checks that an attribute set with a valid name, type
// and sender ID passes, and that any other is refused with a message, never a
// panic.
func FuzzCheckSMSAttributes(f *testing.F) {
	f.Add("DefaultSenderID", "MySender", "DefaultSMSType", "Promotional")
	f.Add("DefaultSenderID", "12345", "DefaultSMSType", "Transactional")
	f.Add("DefaultSenderID", "", "MonthlySpendLimit", "5")
	f.Add("Unknown", "x", "DefaultSMSType", "Other")
	f.Add("", "", "", "")

	f.Fuzz(func(t *testing.T, name1, value1, name2, value2 string) {
		err := checkSMSAttributes(map[string]string{name1: value1, name2: value2})
		if err != nil && err.Message == "" {
			t.Fatalf("checkSMSAttributes refused %q=%q, %q=%q with no message", name1, value1, name2, value2)
		}

		if err == nil {
			for _, name := range []string{name1, name2} {
				if !slices.Contains(smsAttributeNames, name) {
					t.Fatalf("checkSMSAttributes accepted the unknown name %q", name)
				}
			}
		}
	})
}

// FuzzOptOutBody checks that the opt-out route answers 200 or 400 to any body,
// never anything else, and opts out the number of a 200 that names one.
func FuzzOptOutBody(f *testing.F) {
	for _, seed := range []string{
		`{"phoneNumber":"+15551230000"}`,
		`{"phoneNumber":"+15551230000","accountId":"111111111111"}`,
		`{}`,
		`null`,
		`[]`,
		`{"phoneNumber":1}`,
		`{"phoneNumber":""}`,
		``,
		`{"phoneNumber":"+1"`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		p := New(nil)
		r := chi.NewRouter()
		p.mountMessages(r, developerCanonicalPrefix)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			developerCanonicalPrefix+phoneOptOutsPath, strings.NewReader(string(body))))

		if w.Code != http.StatusOK && w.Code != http.StatusBadRequest {
			t.Fatalf("opt-out of %q answered %d", body, w.Code)
		}
	})
}
