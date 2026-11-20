package dynamodb

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// Key types.
const (
	keyHash  = "HASH"
	keyRange = "RANGE"
)

// The messages DynamoDB Local answers for a table definition it refuses, as
// LocalStack's image returns them.
const (
	msgTableName = "Invalid table/index name.  Table/index names must be between 3 and 255 characters long, " +
		"and may contain only the characters a-z, A-Z, 0-9, '_', '-', and '.'"
	msgPayPerRequestThroughput = "One or more parameter values were invalid: Neither ReadCapacityUnits nor " +
		"WriteCapacityUnits can be specified when BillingMode is PAY_PER_REQUEST"
	msgNoThroughput      = "No provisioned throughput specified for the table"
	msgNoIndexThroughput = "No provisioned throughput specified for the global secondary index"
	msgNoProjection      = "Indexes must have a projection specified"
	msgNoKeySchema       = "No defined key schema.  A key schema containing at least a hash key must be " +
		"defined for all tables"
	msgNoHashKey     = "No Hash Key specified in schema.  All Dynamo DB tables must have exactly one hash key"
	msgTwoHashKeys   = "Too many hash keys specified.  All Dynamo DB tables must have exactly one hash key"
	msgHashUndefined = "Hash Key not specified in Attribute Definitions.  Type unknown."
	msgRangeUndefind = "Range Key not specified in Attribute Definitions.  Type unknown."
	msgKeyCountMatch = "The number of attributes in key schema must match the number of attributes defined " +
		"in attribute definitions."
	msgKeyType        = "Member must satisfy enum value set: [B, N, S]"
	msgLocalOnHash    = "Local Secondary indices are not allowed on hash tables, only hash and range tables"
	msgTableNotFound  = "Cannot do operations on a non-existent table"
	msgTableExists    = "Table already exists: "
	msgReturnValues   = "Return values set to invalid value"
	minTableNameBytes = 3
	maxTableNameBytes = 255
)

// isPayPerRequest reports whether a BillingMode is PAY_PER_REQUEST.
func isPayPerRequest(mode *string) bool {
	return mode != nil && *mode == billingPayPerRequest
}

// checkName checks a table or index name as DynamoDB Local does.
func checkName(name *string) error {
	if name == nil || len(*name) < minTableNameBytes || len(*name) > maxTableNameBytes {
		return value.Invalid(msgTableName)
	}

	for _, c := range []byte(*name) {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
		if !ok {
			return value.Invalid(msgTableName)
		}
	}

	return nil
}

// CreateTable creates a table, with LocalStack's additions to DynamoDB
// Local's answer: TableId, SSEDescription, TableClassSummary and the GSI fields.
func (p *Provider) CreateTable(ctx context.Context, in *api.CreateTableInput) (*api.CreateTableOutput, error) {
	c := p.caller(ctx)
	if in.TableName != nil {
		if home, _ := p.home(c, *in.TableName); home.table(*in.TableName) != nil {
			return nil, api.NewResourceInUseException(msgTableExists + *in.TableName)
		}
	}

	if isPayPerRequest(in.BillingMode) && in.ProvisionedThroughput != nil {
		return nil, value.Invalid(msgPayPerRequestThroughput)
	}

	err := checkCreate(in)
	if err != nil {
		return nil, err
	}

	err = checkStream(in.StreamSpecification)
	if err != nil {
		return nil, err
	}

	now := p.now()
	meta := newMeta(in, now)
	meta.SSE = c.sseDescription(in.SSESpecification)

	entry := &tableEntry{meta: meta, data: meta.newEngine(), notify: p.streamsChanged}
	if meta.Stream != nil {
		entry.applyStream(now, p.now, c.region)
	}

	if !c.state.add(meta.Name, entry) {
		return nil, api.NewResourceInUseException(msgTableExists + meta.Name)
	}

	p.account(c).setTags(c.tableARN(meta.Name), in.Tags)

	d := entry.description(c.tableARN(meta.Name))
	d.WarmThroughput = nil

	if in.WarmThroughput != nil {
		d.WarmThroughput = &api.TableWarmThroughputDescription{
			ReadUnitsPerSecond:  in.WarmThroughput.ReadUnitsPerSecond,
			WriteUnitsPerSecond: in.WarmThroughput.WriteUnitsPerSecond,
			Status:              new(statusUpdating),
		}
	}

	if d.BillingModeSummary != nil {
		d.BillingModeSummary.LastUpdateToPayPerRequestDateTime = nil
	}

	if in.StreamSpecification != nil {
		d.StreamSpecification = in.StreamSpecification
	}

	if meta.SSE != nil && meta.SSE.Status == nil {
		d.SSEDescription = meta.SSE
	}

	return &api.CreateTableOutput{TableDescription: d}, nil
}

// newMeta is the definition of the table in, which checkCreate accepted.
func newMeta(in *api.CreateTableInput, now time.Time) tableMeta {
	payPerRequest := isPayPerRequest(in.BillingMode)

	meta := tableMeta{
		Name:               *in.TableName,
		ID:                 uuid(),
		Created:            now,
		Attributes:         in.AttributeDefinitions,
		Key:                in.KeySchema,
		PayPerRequest:      payPerRequest,
		DeletionProtection: in.DeletionProtectionEnabled != nil && *in.DeletionProtectionEnabled,
		Warm:               in.WarmThroughput,
		OnDemand:           in.OnDemandThroughput,
	}

	if payPerRequest {
		meta.PayPerRequestSince = now
	} else {
		meta.Read, meta.Write = capacity(in.ProvisionedThroughput)
	}

	for _, index := range in.LocalSecondaryIndexes {
		meta.Local = append(meta.Local, indexMeta{
			Name: *index.IndexName, Key: index.KeySchema, Projection: *index.Projection,
		})
	}

	for _, index := range in.GlobalSecondaryIndexes {
		meta.Global = append(meta.Global, meta.globalIndex(&index))
	}

	// A new table has no stream to refuse enabling or disabling against.
	_, _ = meta.setStream(in.StreamSpecification, now, false)

	if in.TableClass != nil {
		meta.Class = *in.TableClass
	}

	return meta
}

// globalIndex is the definition of a global secondary index of m.
func (m *tableMeta) globalIndex(index *api.GlobalSecondaryIndex) indexMeta {
	out := indexMeta{
		Name: *index.IndexName, Key: index.KeySchema, Projection: *index.Projection, OnDemand: index.OnDemandThroughput,
	}
	if !m.PayPerRequest {
		out.Read, out.Write = capacity(index.ProvisionedThroughput)
	}

	return out
}

func capacity(throughput *api.ProvisionedThroughput) (read, write int64) {
	if throughput == nil {
		return 0, 0
	}

	if throughput.ReadCapacityUnits != nil {
		read = *throughput.ReadCapacityUnits
	}

	if throughput.WriteCapacityUnits != nil {
		write = *throughput.WriteCapacityUnits
	}

	return read, write
}

// sseDescription is LocalStack's SSEUtils.get_sse_description. A specification
// that is not enabled describes as an empty SSEDescription. One enabled without
// a key would take a KMS key LocalStack creates; gostack has no KMS yet and
// leaves KMSMasterKeyArn out.
func (c caller) sseDescription(spec *api.SSESpecification) *api.SSEDescription {
	if spec == nil {
		return nil
	}

	if spec.Enabled == nil || !*spec.Enabled {
		return &api.SSEDescription{}
	}

	d := &api.SSEDescription{Status: new("ENABLED"), SSEType: new("KMS")}
	if spec.KMSMasterKeyId != nil {
		d.KMSMasterKeyArn = new(c.kmsKeyARN(*spec.KMSMasterKeyId))
	}

	return d
}

// kmsKeyARN is LocalStack's arns.kms_key_arn: a key id in the caller's
// account and region, or an ARN as it is.
func (c caller) kmsKeyARN(key string) string {
	if strings.HasPrefix(key, "arn:") {
		return key
	}

	return "arn:" + partition(c.region) + ":kms:" + c.region + ":" + c.account + ":key/" + key
}

// The version and variant bits of a random UUID.
const (
	uuidVersionMask = 0x0f
	uuidVersion4    = 0x40
	uuidVariantMask = 0x3f
	uuidVariantRFC  = 0x80
)

// uuid returns a random version 4 UUID.
func uuid() string {
	var b [16]byte

	_, _ = rand.Read(b[:])
	b[6] = b[6]&uuidVersionMask | uuidVersion4
	b[8] = b[8]&uuidVariantMask | uuidVariantRFC

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// checkCreate checks a table definition as DynamoDB Local does.
func checkCreate(in *api.CreateTableInput) error {
	payPerRequest := isPayPerRequest(in.BillingMode)

	err := checkName(in.TableName)
	if err != nil {
		return err
	}

	err = checkKeySchema(in.KeySchema, in.AttributeDefinitions)
	if err != nil {
		return err
	}

	err = checkAttributeCount(in)
	if err != nil {
		return err
	}

	if !payPerRequest && in.ProvisionedThroughput == nil {
		return value.Invalid(msgNoThroughput)
	}

	if len(in.LocalSecondaryIndexes) > 0 && len(in.KeySchema) < 2 {
		return value.Invalid(msgLocalOnHash)
	}

	for _, index := range in.LocalSecondaryIndexes {
		err := checkIndex(index.IndexName, index.KeySchema, index.Projection, in.AttributeDefinitions)
		if err != nil {
			return err
		}
	}

	for _, index := range in.GlobalSecondaryIndexes {
		err := checkIndex(index.IndexName, index.KeySchema, index.Projection, in.AttributeDefinitions)
		if err != nil {
			return err
		}

		if !payPerRequest && index.ProvisionedThroughput == nil {
			return value.Invalid(msgNoIndexThroughput)
		}
	}

	return nil
}

// checkKeySchema checks a key schema: a hash key first, at most one range key
// after it, each defined with a scalar type.
func checkKeySchema(key []api.KeySchemaElement, defs []api.AttributeDefinition) error {
	if len(key) == 0 {
		return value.Invalid(msgNoKeySchema)
	}

	for i, element := range key {
		if element.KeyType == nil || element.AttributeName == nil {
			return value.Invalid(msgNoHashKey)
		}

		switch {
		case i == 0 && *element.KeyType != keyHash:
			return value.Invalid(msgNoHashKey)
		case i > 0 && *element.KeyType == keyHash:
			return value.Invalid(msgTwoHashKeys)
		case i > 1:
			return value.Invalid(msgKeyCountMatch)
		}
	}

	for i, element := range key {
		kind, found := definedType(defs, *element.AttributeName)
		switch {
		case !found && i == 0:
			return value.Invalid(msgHashUndefined)
		case !found:
			return value.Invalid(msgRangeUndefind)
		case kind != value.TypeS && kind != value.TypeN && kind != value.TypeB:
			return value.Invalid(msgKeyType)
		}
	}

	return nil
}

func definedType(defs []api.AttributeDefinition, name string) (string, bool) {
	for _, def := range defs {
		if def.AttributeName != nil && *def.AttributeName == name {
			if def.AttributeType == nil {
				return "", true
			}

			return *def.AttributeType, true
		}
	}

	return "", false
}

// checkAttributeCount checks that every attribute definition is a key
// attribute of the table or of one of its indexes.
func checkAttributeCount(in *api.CreateTableInput) error {
	used := map[string]bool{}
	addKey := func(key []api.KeySchemaElement) {
		for _, element := range key {
			if element.AttributeName != nil {
				used[*element.AttributeName] = true
			}
		}
	}

	addKey(in.KeySchema)

	for _, index := range in.LocalSecondaryIndexes {
		addKey(index.KeySchema)
	}

	for _, index := range in.GlobalSecondaryIndexes {
		addKey(index.KeySchema)
	}

	if len(used) != len(in.AttributeDefinitions) {
		return value.Invalid(msgKeyCountMatch)
	}

	return nil
}

// checkIndex checks a secondary index's name, key and projection.
func checkIndex(
	name *string, key []api.KeySchemaElement, projection *api.Projection, defs []api.AttributeDefinition,
) error {
	err := checkName(name)
	if err != nil {
		return err
	}

	err = checkKeySchema(key, defs)
	if err != nil {
		return err
	}

	if projection == nil {
		return value.Invalid(msgNoProjection)
	}

	return nil
}
