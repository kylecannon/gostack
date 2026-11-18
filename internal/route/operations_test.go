package route_test

import (
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/route"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withOperations returns meta with its operation list set.
func withOperations(meta service.Metadata, operations ...string) service.Metadata {
	meta.Operations = operations

	return meta
}

// Test_Detect_byOperation is LocalStack's service_router.py step 5: an Action
// with no other hint belongs to the query services that have the operation, and
// the Version only breaks a tie.
func Test_Detect_byOperation(t *testing.T) {
	const formType = "application/x-www-form-urlencoded"

	sqsOps := withOperations(sqs, "ListQueues", "SendMessage")
	stsOps := withOperations(sts, "GetCallerIdentity", "AssumeRole")
	newer := withOperations(sns, "ListQueues")
	newer.APIVersion = "2013-01-01"

	tests := []struct {
		name         string
		giveServices []service.Metadata
		giveForm     string
		wantService  string
		wantErr      error
		wantServices []string
	}{
		{
			name:         "ListQueues without Version is sqs",
			giveServices: []service.Metadata{sqsOps, stsOps},
			giveForm:     formListQueues,
			wantService:  nameSQS,
		},
		{
			name:         "GetCallerIdentity without Version is sts",
			giveServices: []service.Metadata{sqsOps, stsOps},
			giveForm:     formGetCallerIdentity,
			wantService:  nameSTS,
		},
		{
			name:         "the registrations in the other order",
			giveServices: []service.Metadata{stsOps, sqsOps},
			giveForm:     formListQueues,
			wantService:  nameSQS,
		},
		{
			name:         "an Action that no service has leaves nothing",
			giveServices: []service.Metadata{sqsOps, stsOps},
			giveForm:     "Action=NoSuchOperation",
			wantErr:      route.ErrNoService,
		},
		{
			name:         "the Version breaks a tie between services that have the operation",
			giveServices: []service.Metadata{sqsOps, newer},
			giveForm:     "Action=ListQueues&Version=2013-01-01",
			wantService:  nameSNS,
		},
		{
			name:         "a real tie is still ambiguous",
			giveServices: []service.Metadata{sqsOps, newer},
			giveForm:     formListQueues,
			wantServices: []string{nameSNS, nameSQS},
		},
		{
			name:         "a service with no operation list stays a candidate",
			giveServices: []service.Metadata{sqsOps, sns},
			giveForm:     formListQueues,
			wantServices: []string{nameSNS, nameSQS},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, err := route.NewTable(tt.giveServices)
			require.NoError(t, err)

			headers := map[string]string{headerType: formType}

			got, err := table.Detect(newRequest(t, localHost, headers), formOf(tt.giveForm))

			if tt.wantServices != nil {
				var ambiguous *route.AmbiguousError

				require.ErrorAs(t, err, &ambiguous)
				assert.Equal(t, tt.wantServices, ambiguous.Services)

				return
			}

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantService, got.Service)
			assert.Equal(t, protocol.KindQuery, got.Protocol)
		})
	}
}
