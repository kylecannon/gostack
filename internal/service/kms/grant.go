package kms

import (
	"net/http"
	"slices"
	"sync"
	"time"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
)

// Grant is a KMS grant: LocalStack's KmsGrant. It is stored and listed, and
// never enforced, as in LocalStack: no IAM policy or key policy is evaluated
// anywhere in KMS, so a grant gives no permission a call could lack.
type Grant struct {
	ID                string
	KeyID             string
	KeyARN            string
	Name              string
	Created           time.Time
	GranteePrincipal  string
	RetiringPrincipal string
	IssuingAccount    string
	Operations        []string
	Constraints       *api.GrantConstraints
	// Tokens are the grant tokens CreateGrant answered for the grant. AWS says
	// the token of every call is new and all of them name the same grant;
	// LocalStack keeps only the first.
	Tokens []string
}

// grantStore holds the grants of one account and region in the order they were
// made, since LocalStack lists and pages them in that order, and the index of
// their tokens. mu guards all of it; a Grant is not changed after it is stored,
// except that Tokens grows, under mu. The zero value is an empty store.
type grantStore struct {
	mu     sync.Mutex
	grants map[string]*Grant
	order  []string
	// tokens maps a grant token to the id of its grant.
	tokens map[string]string
}

// reset empties the store.
func (s *grantStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.grants = nil
	s.order = nil
	s.tokens = nil
}

// add stores g under token. A grant with a name that another grant of the same
// key already has is not stored: the token joins the existing grant, and its id
// is the answer, as LocalStack's name check does. It returns the id of the
// grant the token names.
func (s *grantStore) add(g *Grant, token string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.grants == nil {
		s.grants = map[string]*Grant{}
		s.tokens = map[string]string{}
	}

	if g.Name != "" {
		for _, id := range s.order {
			have := s.grants[id]
			if have.Name == g.Name && have.KeyID == g.KeyID {
				have.Tokens = append(have.Tokens, token)
				s.tokens[token] = have.ID

				return have.ID
			}
		}
	}

	g.Tokens = []string{token}
	s.grants[g.ID] = g
	s.order = append(s.order, g.ID)
	s.tokens[token] = g.ID

	return g.ID
}

// get returns a copy of the grant with id.
func (s *grantStore) get(id string) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.grants[id]
	if !ok {
		return Grant{}, false
	}

	return g.copy(), true
}

// list returns a copy of each grant keep accepts, in the order they were made.
func (s *grantStore) list(keep func(*Grant) bool) []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Grant, 0, len(s.order))

	for _, id := range s.order {
		if g := s.grants[id]; keep(g) {
			out = append(out, g.copy())
		}
	}

	return out
}

// idOfToken returns the id of the grant a token names.
func (s *grantStore) idOfToken(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, ok := s.tokens[token]

	return id, ok
}

// remove deletes the grant with id, with all its tokens, after LocalStack's
// _delete_grant: the grant must exist and, when keyID is not empty, be a grant
// of that key. keyID names the key in the error.
func (s *grantStore) remove(id, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.grants[id]
	if !ok {
		return api.NewInvalidGrantIdException("")
	}

	if keyID != "" && g.KeyID != keyID {
		return grantValidation("Invalid KeyId=" + keyID + " specified for grant " + id)
	}

	for _, token := range g.Tokens {
		delete(s.tokens, token)
	}

	delete(s.grants, id)
	s.order = slices.DeleteFunc(s.order, func(o string) bool { return o == id })

	return nil
}

// copy returns g with its own Tokens, for a reader that holds no lock.
func (g *Grant) copy() Grant {
	out := *g
	out.Tokens = slices.Clone(g.Tokens)

	return out
}

// entry is the GrantListEntry of g.
func (g *Grant) entry() api.GrantListEntry {
	e := api.GrantListEntry{
		KeyId:            new(g.KeyARN),
		GrantId:          new(g.ID),
		Name:             new(g.Name),
		CreationDate:     &protocol.Time{Time: g.Created},
		GranteePrincipal: new(g.GranteePrincipal),
		IssuingAccount:   new(g.IssuingAccount),
		Operations:       g.Operations,
		Constraints:      g.Constraints,
	}

	if g.RetiringPrincipal != "" {
		e.RetiringPrincipal = new(g.RetiringPrincipal)
	}

	return e
}

// grantValidation is the ValidationError LocalStack's grant calls raise, a
// code that is not in the model but is the one AWS documents for CreateGrant.
func grantValidation(message string) *awserr.Error {
	return &awserr.Error{
		Code:       "ValidationError",
		Message:    message,
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultSender,
	}
}
