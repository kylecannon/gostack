package dynamodb

import (
	"fmt"
	"sync"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
)

// Billing modes.
const (
	billingProvisioned   = "PROVISIONED"
	billingPayPerRequest = "PAY_PER_REQUEST"
)

// LocalStack's default WarmThroughput, by billing mode.
const (
	provisionedWarmUnits = 5
	onDemandWarmRead     = 12000
	onDemandWarmWrite    = 4000
)

// Table and index statuses.
const (
	statusActive   = "ACTIVE"
	statusCreating = "CREATING"
	statusDeleting = "DELETING"
	statusUpdating = "UPDATING"
)

// State is one account's tables in one region.
type State struct {
	// mu guards the map; each table guards itself.
	mu     sync.RWMutex
	tables map[string]*tableEntry

	// tokens are the TransactWriteItems ClientRequestTokens in their window.
	tokens tokens
}

func newState() *State {
	return &State{tables: map[string]*tableEntry{}}
}

func (s *State) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.tables {
		e.stopStream()
	}

	s.tables = map[string]*tableEntry{}
	s.tokens.reset()
}

// table returns the named table, or nil.
func (s *State) table(name string) *tableEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.tables[name]
}

// add stores e under name and reports whether it did: false when a table of
// that name exists.
func (s *State) add(name string, e *tableEntry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tables[name] != nil {
		return false
	}

	s.tables[name] = e

	return true
}

// remove deletes the named table and returns it, once check accepts it, with
// DeleteTable's own message when there is none. check runs under the map's
// lock, so no other delete can race it.
func (s *State) remove(name string, check func(*tableEntry) error) (*tableEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.tables[name]
	if e == nil {
		return nil, api.NewResourceNotFoundException(fmt.Sprintf(msgDeleteNotFound, name))
	}

	err := check(e)
	if err != nil {
		return nil, err
	}

	delete(s.tables, name)

	return e, nil
}

// protected reports whether the table has deletion protection on.
func (e *tableEntry) protected() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.meta.DeletionProtection
}

// namesAfter returns the table names that sort after start, or every name
// when start is nil, in no order.
func (s *State) namesAfter(start *string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.tables))

	for name := range s.tables {
		if start == nil || name > *start {
			names = append(names, name)
		}
	}

	return names
}

// tableEntry is one table: its definition and its items.
type tableEntry struct {
	// mu guards meta. The engine has its own lock.
	mu   sync.RWMutex
	meta tableMeta
	data *table.Table

	// shard is the latest stream's shard, nil when the table never had a
	// stream; closed once the stream is disabled. viewType and streamRegion
	// are the view type and awsRegion it was enabled with. Guarded by mu.
	shard        *stream.Log
	viewType     string
	streamRegion string
}

// tableMeta is a table's definition, as CreateTable and UpdateTable set it.
type tableMeta struct {
	Name       string
	ID         string
	Created    time.Time
	Attributes []api.AttributeDefinition
	Key        []api.KeySchemaElement
	// PayPerRequest is the billing mode; PayPerRequestSince is when it was set.
	PayPerRequest      bool
	PayPerRequestSince time.Time
	Read               int64
	Write              int64
	Local              []indexMeta
	Global             []indexMeta
	// Stream is the stream specification as given; StreamLabel names the
	// stream while one is enabled.
	Stream      *api.StreamSpecification
	StreamLabel string
	// SSE is the description of the encryption asked for, nil when
	// SSESpecification was not given.
	SSE                *api.SSEDescription
	Class              string
	DeletionProtection bool
	Warm               *api.WarmThroughput
	OnDemand           *api.OnDemandThroughput
	Tags               []api.Tag
}

// indexMeta is a secondary index's definition.
type indexMeta struct {
	Name       string
	Key        []api.KeySchemaElement
	Projection api.Projection
	Read       int64
	Write      int64
	OnDemand   *api.OnDemandThroughput
}

// attributeType returns the type of the named attribute, or "".
func (m *tableMeta) attributeType(name string) string {
	for _, def := range m.Attributes {
		if def.AttributeName != nil && *def.AttributeName == name && def.AttributeType != nil {
			return *def.AttributeType
		}
	}

	return ""
}

// keySchema converts a key schema whose attributes are all defined.
func (m *tableMeta) keySchema(key []api.KeySchemaElement) table.KeySchema {
	var out table.KeySchema

	for _, element := range key {
		attr := table.KeyAttribute{Name: *element.AttributeName, Type: m.attributeType(*element.AttributeName)}
		if *element.KeyType == keyHash {
			out.Hash = attr
		} else {
			out.Range = attr
		}
	}

	return out
}

// indexSpec is the engine's form of an index.
func (m *tableMeta) indexSpec(index *indexMeta, global bool) table.IndexSpec {
	projection := table.Projection{NonKeyAttributes: index.Projection.NonKeyAttributes}
	if index.Projection.ProjectionType != nil {
		projection.Type = *index.Projection.ProjectionType
	}

	return table.IndexSpec{Name: index.Name, Global: global, Key: m.keySchema(index.Key), Projection: projection}
}

// newEngine builds the table engine for m.
func (m *tableMeta) newEngine() *table.Table {
	specs := make([]table.IndexSpec, 0, len(m.Local)+len(m.Global))
	for i := range m.Local {
		specs = append(specs, m.indexSpec(&m.Local[i], false))
	}

	for i := range m.Global {
		specs = append(specs, m.indexSpec(&m.Global[i], true))
	}

	return table.New(m.keySchema(m.Key), specs)
}

// streamARN is the ARN of the table's stream.
func (m *tableMeta) streamARN(tableARN string) string {
	return tableARN + "/stream/" + m.StreamLabel
}

// description is a table's TableDescription, as DescribeTable returns it.
func (e *tableEntry) description(arn string) *api.TableDescription {
	e.mu.RLock()
	defer e.mu.RUnlock()

	m := &e.meta
	count, size := e.data.Stats()
	d := &api.TableDescription{
		AttributeDefinitions:      m.Attributes,
		TableName:                 new(m.Name),
		KeySchema:                 m.Key,
		TableStatus:               new(statusActive),
		CreationDateTime:          timestamp(m.Created),
		ProvisionedThroughput:     throughput(m.Read, m.Write),
		TableSizeBytes:            new(int64(size)),
		ItemCount:                 new(int64(count)),
		TableArn:                  new(arn),
		TableId:                   new(m.ID),
		DeletionProtectionEnabled: new(m.DeletionProtection),
		OnDemandThroughput:        m.OnDemand,
	}

	if m.PayPerRequest {
		d.BillingModeSummary = &api.BillingModeSummary{
			BillingMode:                       new(billingPayPerRequest),
			LastUpdateToPayPerRequestDateTime: timestamp(m.PayPerRequestSince),
		}
	}

	if len(m.Local) > 0 {
		d.LocalSecondaryIndexes = e.localIndexes(arn)
	}
	if len(m.Global) > 0 {
		d.GlobalSecondaryIndexes = e.globalIndexes(arn)
	}

	if m.StreamLabel != "" {
		d.StreamSpecification = m.Stream // nil once the stream is disabled
		d.LatestStreamLabel = new(m.StreamLabel)
		d.LatestStreamArn = new(m.streamARN(arn))
	}

	if m.SSE != nil && m.SSE.Status != nil {
		d.SSEDescription = m.SSE
	}

	if m.Class != "" {
		d.TableClassSummary = &api.TableClassSummary{TableClass: new(m.Class)}
	}

	d.WarmThroughput = m.warmThroughput()

	return d
}

// warmThroughput is the table's WarmThroughput: the one given at creation, or
// LocalStack's defaults for the billing mode.
func (m *tableMeta) warmThroughput() *api.TableWarmThroughputDescription {
	if m.Warm != nil {
		return &api.TableWarmThroughputDescription{
			ReadUnitsPerSecond:  m.Warm.ReadUnitsPerSecond,
			WriteUnitsPerSecond: m.Warm.WriteUnitsPerSecond,
			Status:              new(statusActive),
		}
	}

	read, write := int64(provisionedWarmUnits), int64(provisionedWarmUnits)
	if m.PayPerRequest {
		read, write = 12000, 4000
	}

	return &api.TableWarmThroughputDescription{
		ReadUnitsPerSecond:  new(read),
		WriteUnitsPerSecond: new(write),
		Status:              new(statusActive),
	}
}

func (e *tableEntry) localIndexes(arn string) []api.LocalSecondaryIndexDescription {
	out := make([]api.LocalSecondaryIndexDescription, 0, len(e.meta.Local))

	for i := range e.meta.Local {
		index := &e.meta.Local[i]
		count, size, _ := e.data.IndexStats(index.Name)
		out = append(out, api.LocalSecondaryIndexDescription{
			IndexName:      new(index.Name),
			KeySchema:      index.Key,
			Projection:     &index.Projection,
			IndexSizeBytes: new(int64(size)),
			ItemCount:      new(int64(count)),
			IndexArn:       new(arn + "/index/" + index.Name),
		})
	}

	return out
}

func (e *tableEntry) globalIndexes(arn string) []api.GlobalSecondaryIndexDescription {
	out := make([]api.GlobalSecondaryIndexDescription, 0, len(e.meta.Global))

	for i := range e.meta.Global {
		index := &e.meta.Global[i]
		count, size, _ := e.data.IndexStats(index.Name)
		out = append(out, api.GlobalSecondaryIndexDescription{
			IndexName:             new(index.Name),
			KeySchema:             index.Key,
			Projection:            &index.Projection,
			IndexStatus:           new(statusActive),
			ProvisionedThroughput: throughput(index.Read, index.Write),
			IndexSizeBytes:        new(int64(size)),
			ItemCount:             new(int64(count)),
			IndexArn:              new(arn + "/index/" + index.Name),
			OnDemandThroughput:    index.OnDemand,
		})
	}

	return out
}

func throughput(read, write int64) *api.ProvisionedThroughputDescription {
	return &api.ProvisionedThroughputDescription{
		NumberOfDecreasesToday: new(int64(0)),
		ReadCapacityUnits:      new(read),
		WriteCapacityUnits:     new(write),
	}
}

func timestamp(t time.Time) *protocol.Time {
	return &protocol.Time{Time: t}
}
