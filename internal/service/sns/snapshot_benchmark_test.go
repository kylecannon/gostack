package sns_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service/sns"
)

// The persistence rows of the SNS matrix (docs/standards.md, "Benchmarks",
// axis "Persistence"): Publish under persist=on (BenchmarkPublish), and the
// snapshot and restore of a region holding 10, 1,000 or 100,000
// subscriptions.
//
//	BenchmarkSnapshot/subscriptions=100000
//	BenchmarkRestore/subscriptions=100000

// persistFrequency is the flush delay of a persisted bench: short enough that
// a save is always running or due while the calls run.
const persistFrequency = 10 * time.Millisecond

var snapshotSizes = []int{10, 1_000, 100_000}

// persistedBenchEngine is an engine under a temp dir, as PERSIST_SNS=1 wires
// one. It is closed when the benchmark ends.
func persistedBenchEngine(b *testing.B) *persist.Engine {
	b.Helper()

	dir := b.TempDir()
	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: persistFrequency,
		MountDetector: func(string) bool { return false },
	})

	b.Cleanup(func() {
		err := eng.Close(context.WithoutCancel(b.Context()))
		if err != nil {
			b.Errorf("close the engine: %v", err)
		}

		_, err = os.Stat(filepath.Join(dir, snsName, "store.json"))
		if err != nil {
			b.Errorf("SNS saved no snapshot: %v", err)
		}
	})

	return eng
}

// registerPersisted registers p with eng and loads it.
func registerPersisted(b *testing.B, eng *persist.Engine, p *sns.Provider) {
	b.Helper()

	eng.Register(snsName, p.Registration().Persist)

	err := eng.Load(b.Context())
	if err != nil {
		b.Fatal(err)
	}
}

// benchSnapshot is a snapshot of one region holding count subscriptions to
// SQS queues, a hundred to a topic, every other one with a filter policy.
// It is written as Snapshot writes one, so the benchmark needs no 100,000
// Subscribe calls to build it.
func benchSnapshot(count int) []byte {
	const perTopic = 100

	var topics, subs strings.Builder

	for i := range (count + perTopic - 1) / perTopic {
		if i > 0 {
			topics.WriteByte(',')
		}

		name := "bench-" + strconv.Itoa(i)
		topics.WriteString(`{"name":"` + name + `","arn":"` + arnPrefix + name + `","attributes":{"DisplayName":""}}`)
	}

	for i := range count {
		if i > 0 {
			subs.WriteByte(',')
		}

		n := strconv.Itoa(i)
		topic := arnPrefix + "bench-" + strconv.Itoa(i/perTopic)
		subs.WriteString(`{"token":"` + strconv.FormatInt(int64(i), 16) + `","topicArn":"` + topic +
			`","endpoint":"arn:aws:sqs:` + region + ":" + account + ":q" + n + `","protocol":"sqs","arn":"` +
			topic + ":" + n + `","pendingConfirmation":"false","owner":"` + account +
			`","subscriptionPrincipal":"p","filterPolicyScope":"MessageAttributes","rawMessageDelivery":"false"`)

		if i%2 == 1 {
			subs.WriteString(`,"filterPolicy":"{\"color\":[\"red\"]}"`)
		}

		subs.WriteByte('}')
	}

	return []byte(`{"v":1,"data":{"sequence":0,"states":[{"account":"` + account + `","region":"` + region +
		`","topics":[` + topics.String() + `],"subscriptions":[` + subs.String() + `]}],"accounts":[]}}`)
}

// filledSNS is a provider holding count subscriptions.
func filledSNS(b *testing.B, count int) *sns.Provider {
	b.Helper()

	p := sns.New(nil, nil)

	err := p.Restore(bytes.NewReader(benchSnapshot(count)))
	if err != nil {
		b.Fatal(err)
	}

	return p
}

func BenchmarkSnapshot(b *testing.B) {
	for _, count := range snapshotSizes {
		b.Run("subscriptions="+strconv.Itoa(count), func(b *testing.B) {
			p := filledSNS(b, count)

			var size bytes.Buffer

			err := p.Snapshot(&size)
			if err != nil {
				b.Fatal(err)
			}

			b.SetBytes(int64(size.Len()))
			b.ReportAllocs()

			for b.Loop() {
				err = p.Snapshot(io.Discard)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRestore(b *testing.B) {
	for _, count := range snapshotSizes {
		b.Run("subscriptions="+strconv.Itoa(count), func(b *testing.B) {
			var saved bytes.Buffer

			err := filledSNS(b, count).Snapshot(&saved)
			if err != nil {
				b.Fatal(err)
			}

			p := sns.New(nil, nil)

			b.SetBytes(int64(saved.Len()))
			b.ReportAllocs()

			for b.Loop() {
				err = p.Restore(bytes.NewReader(saved.Bytes()))
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
