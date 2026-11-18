package main

import (
	"fmt"
	"strconv"
)

// The cells of the system matrix (docs/standards.md, "Benchmarks"). A cell is
// one combination of the axes below, measured by running the same cycle at a
// concurrency for a fixed time, several times, each in a container of its own.

const (
	queueStandard = "standard"
	queueFIFO     = "fifo"

	shapeSingle   = "single"
	shapeBatch    = "batch"
	shapeLongPoll = "longpoll"

	protoJSON  = "json"
	protoQuery = "query"

	profileFull  = "full"
	profileQuick = "quick"

	// batchSize is the entries of a batch call, and the messages a batch cycle
	// moves.
	batchSize = 10
	// maxBatchBytes is the service's limit on the bodies of one batch call.
	maxBatchBytes = 1 << 20
	// longPollSeconds is WaitTimeSeconds of the long-poll shape.
	longPollSeconds = 1

	payloadSmall  = 256
	payloadMedium = 64 << 10
	payloadMax    = 1 << 20

	bytesPerKiB = 1 << 10

	// fullCellsPerPersistence is 3 payloads x 2 queues x 3 shapes x 2 protocols x 3 concurrencies.
	fullCellsPerPersistence = 108
)

// cellSpec is one point of the matrix.
type cellSpec struct {
	Payload     int    `json:"payload_bytes"`
	Queue       string `json:"queue"`
	Shape       string `json:"shape"`
	Proto       string `json:"proto"`
	Persist     bool   `json:"persist"`
	Concurrency int    `json:"concurrency"`
}

// payloadName writes a size the way the cell id carries it.
func payloadName(n int) string {
	switch {
	case n >= bytesPerMiB && n%bytesPerMiB == 0:
		return strconv.Itoa(n/bytesPerMiB) + "MiB"
	case n >= bytesPerKiB && n%bytesPerKiB == 0:
		return strconv.Itoa(n/bytesPerKiB) + "KiB"
	default:
		return strconv.Itoa(n) + "B"
	}
}

// id is the name a cell has in the result files, and the key compare matches
// cells by. It is the same in every profile.
func (c cellSpec) id() string {
	persist := "off"
	if c.Persist {
		persist = "on"
	}

	return fmt.Sprintf("persist=%s/payload=%s/queue=%s/shape=%s/proto=%s/c=%d",
		persist, payloadName(c.Payload), c.Queue, c.Shape, c.Proto, c.Concurrency)
}

// entryBytes is the body of one message of the cell. A batch carries ten, and
// the service takes 1 MiB of bodies in one call, so a batch of the largest
// payload shrinks each entry.
func (c cellSpec) entryBytes() int {
	if c.Shape == shapeBatch {
		return min(c.Payload, maxBatchBytes/batchSize)
	}

	return c.Payload
}

// baseCell is the cell the quick profile varies one axis of at a time.
func baseCell() cellSpec {
	return cellSpec{
		Payload: payloadSmall, Queue: queueStandard, Shape: shapeSingle, Proto: protoJSON, Concurrency: levelMid,
	}
}

const (
	levelLow  = 1
	levelMid  = 16
	levelHigh = 64
)

// fullCells is every cell of one persistence setting: payload, queue, shape,
// protocol and concurrency.
func fullCells(persist bool) []cellSpec {
	cells := make([]cellSpec, 0, fullCellsPerPersistence)

	for _, payload := range []int{payloadSmall, payloadMedium, payloadMax} {
		for _, queue := range []string{queueStandard, queueFIFO} {
			for _, shape := range []string{shapeSingle, shapeBatch, shapeLongPoll} {
				for _, proto := range []string{protoJSON, protoQuery} {
					for _, level := range []int{levelLow, levelMid, levelHigh} {
						cells = append(cells, cellSpec{
							Payload: payload, Queue: queue, Shape: shape, Proto: proto, Persist: persist, Concurrency: level,
						})
					}
				}
			}
		}
	}

	return cells
}

// quickCells covers each axis once at concurrency 16 around the base cell, and
// adds the base cell at concurrency 1 and 64. Every quick cell is a cell of the
// full profile, with the same id.
func quickCells() []cellSpec {
	base := baseCell()
	vary := func(change func(*cellSpec)) cellSpec {
		c := base
		change(&c)

		return c
	}

	return []cellSpec{
		vary(func(c *cellSpec) { c.Concurrency = levelLow }),
		base,
		vary(func(c *cellSpec) { c.Concurrency = levelHigh }),
		vary(func(c *cellSpec) { c.Payload = payloadMedium }),
		vary(func(c *cellSpec) { c.Payload = payloadMax }),
		vary(func(c *cellSpec) { c.Queue = queueFIFO }),
		vary(func(c *cellSpec) { c.Shape = shapeBatch }),
		vary(func(c *cellSpec) { c.Shape = shapeLongPoll }),
		vary(func(c *cellSpec) { c.Proto = protoQuery }),
		vary(func(c *cellSpec) { c.Persist = true }),
	}
}

// cellsFor lists the cells of a profile that a target runs. A target that does
// not persist leaves the persistence-on cells out.
func cellsFor(profile string, t target) []cellSpec {
	var all []cellSpec

	if profile == profileQuick {
		all = quickCells()
	} else {
		all = append(fullCells(false), fullCells(true)...)
	}

	var out []cellSpec

	for _, c := range all {
		if c.Persist && !t.persistable {
			continue
		}

		out = append(out, c)
	}

	return out
}
