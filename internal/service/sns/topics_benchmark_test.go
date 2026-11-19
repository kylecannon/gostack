package sns_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
)

// The topic benchmarks send query calls through the real gateway, in process,
// to a region that already holds topics=N topics. Setup that is not the call
// under test (creating the N topics, deleting what a create benchmark added)
// runs with the timer stopped.
//
//	BenchmarkCreateTopic/topics=1000/topic=fifo/create=new/concurrency=16
//	BenchmarkListTopics/topics=10000/page=middle/concurrency=1

var (
	benchTopics      = []int{10, 1000, 10000}
	benchConcurrency = []int{1, 16}
)

// benchExtra is how many topics a create benchmark adds before it deletes them,
// so the region stays near its topics=N.
const benchExtra = 1000

// benchGateway is SNS behind the real gateway.
type benchGateway struct {
	b       *testing.B
	handler http.Handler
}

func newBenchGateway(b *testing.B) *benchGateway {
	b.Helper()

	provider := sns.New(nil)

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{provider.Registration()}, nil, nil)
	if err != nil {
		b.Fatal(err)
	}

	return &benchGateway{b: b, handler: g.Handler()}
}

// call sends one action and returns its status and the start of its body.
func (g *benchGateway) call(form string) (int, string) {
	req := httptest.NewRequestWithContext(g.b.Context(), http.MethodPost, "http://localhost:4566/",
		strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20130524/"+region+
		"/sns/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc")

	w := httptest.NewRecorder()
	g.handler.ServeHTTP(w, req)

	body := w.Body.String()

	return w.Code, body[:min(len(body), sinkHead)]
}

// must sends one action and fails the benchmark on any status but 200.
func (g *benchGateway) must(form string) {
	if status, body := g.call(form); status != http.StatusOK {
		g.b.Fatalf("%s answered %d: %s", form, status, body)
	}
}

const sinkHead = 512

// createForm is a CreateTopic of name with the attribute pairs attrs.
func createForm(name string, attrs ...string) string {
	form := queryForm("CreateTopic", paramName, name)

	for i := 0; i+1 < len(attrs); i += 2 {
		n := strconv.Itoa(i/2 + 1)
		form.Set("Attributes.entry."+n+".key", attrs[i])
		form.Set("Attributes.entry."+n+".value", attrs[i+1])
	}

	return form.Encode()
}

func deleteForm(name string) string {
	return queryForm("DeleteTopic", "TopicArn", arnPrefix+name).Encode()
}

// fill creates topics bench-0 to bench-<n-1>.
func (g *benchGateway) fill(n int) {
	g.b.Helper()

	for i := range n {
		g.must(createForm("bench-" + strconv.Itoa(i)))
	}
}

// fanOut runs fn on workers goroutines at once.
func fanOut(workers int, fn func(worker int)) {
	var wg sync.WaitGroup

	for worker := range workers {
		wg.Go(func() { fn(worker) })
	}

	wg.Wait()
}

// failed records the first failed call of a fanOut, which the benchmark
// reports after the workers are done, since only its own goroutine may stop it.
type failed struct {
	once sync.Once
	msg  string
}

func (f *failed) check(status int, body string) {
	if status != http.StatusOK {
		f.once.Do(func() { f.msg = strconv.Itoa(status) + ": " + body })
	}
}

// createCell is a point on CreateTopic's option axes.
type createCell struct {
	name string
	// suffix and attrs make a new topic's name and attributes.
	suffix string
	attrs  []string
	// repeat names a topic the region has, instead of a new one.
	repeat bool
}

func BenchmarkCreateTopic(b *testing.B) {
	cells := []createCell{
		{name: "topic=standard/create=new"},
		{name: "topic=standard/create=repeat", repeat: true},
		{name: "topic=fifo/create=new", suffix: ".fifo", attrs: []string{"FifoTopic", textTrue}},
	}

	for _, topics := range benchTopics {
		for _, cell := range cells {
			for _, workers := range benchConcurrency {
				name := fmt.Sprintf("topics=%d/%s/concurrency=%d", topics, cell.name, workers)

				b.Run(name, func(b *testing.B) {
					benchCreateTopic(b, topics, workers, cell)
				})
			}
		}
	}
}

// benchCreateTopic creates workers topics an iteration in a region of topics
// topics. A repeated create names one of those topics again, which compares
// it and changes nothing; a new create names a topic the region does not have,
// and every benchExtra of them are deleted with the timer stopped.
func benchCreateTopic(b *testing.B, topics, workers int, cell createCell) {
	g := newBenchGateway(b)
	g.fill(topics)

	var (
		fail    failed
		created []string
		next    int
	)

	b.ReportAllocs()

	for b.Loop() {
		forms := make([]string, workers)

		for worker := range forms {
			if cell.repeat {
				forms[worker] = createForm("bench-" + strconv.Itoa(worker%topics))

				continue
			}

			name := "new-" + strconv.Itoa(next) + cell.suffix
			next++

			created = append(created, name)
			forms[worker] = createForm(name, cell.attrs...)
		}

		fanOut(workers, func(worker int) { fail.check(g.call(forms[worker])) })

		if fail.msg != "" {
			b.Fatal("CreateTopic answered " + fail.msg)
		}

		if len(created) >= benchExtra {
			b.StopTimer()

			for _, name := range created {
				g.must(deleteForm(name))
			}

			created = created[:0]

			b.StartTimer()
		}
	}
}

func BenchmarkListTopics(b *testing.B) {
	for _, topics := range benchTopics {
		g := newBenchGateway(b)
		g.fill(topics)

		// The middle page starts at the topic in the middle of the creation
		// order, which the token names.
		middle := base64.StdEncoding.EncodeToString([]byte(arnPrefix + "bench-" + strconv.Itoa(topics/2)))

		pages := []struct{ name, form string }{
			{"first", queryForm("ListTopics").Encode()},
			{"middle", queryForm("ListTopics", "NextToken", middle).Encode()},
		}

		for _, page := range pages {
			for _, workers := range benchConcurrency {
				name := fmt.Sprintf("topics=%d/page=%s/concurrency=%d", topics, page.name, workers)

				b.Run(name, func(b *testing.B) {
					g.b = b

					var fail failed

					b.ReportAllocs()

					for b.Loop() {
						fanOut(workers, func(int) { fail.check(g.call(page.form)) })

						if fail.msg != "" {
							b.Fatal("ListTopics answered " + fail.msg)
						}
					}
				})
			}
		}
	}
}
