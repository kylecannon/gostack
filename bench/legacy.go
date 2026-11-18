package main

import (
	"encoding/json"
	"fmt"
)

// Schema 2 result files, the ones committed under bench/results, were taken when
// SQS was the only service. A cell held send, receive and delete as fields, its
// id had no service, and a target held one agentic result. decodeV2 reads such a
// file and returns it as schema 3, so the same files still compare. The cell ids
// it produces are the ones sqsWorkload.Cells emits today.

const schemaV2 = 2

// cellSpecV2 is the matrix point of schema 2.
type cellSpecV2 struct {
	Payload     int    `json:"payload_bytes"`
	Queue       string `json:"queue"`
	Shape       string `json:"shape"`
	Proto       string `json:"proto"`
	Persist     bool   `json:"persist"`
	Concurrency int    `json:"concurrency"`
}

// cellResultV2 is a cell of schema 2.
type cellResultV2 struct {
	ID            string     `json:"id"`
	Spec          cellSpecV2 `json:"spec"`
	Runs          int        `json:"runs"`
	Send          opSpread   `json:"send"`
	Receive       opSpread   `json:"receive"`
	Delete        opSpread   `json:"delete"`
	CyclesPerSec  spread     `json:"cycles_per_sec"`
	PeakRSSBytes  spread     `json:"peak_rss_bytes"`
	CPUPercent    spread     `json:"cpu_percent"`
	EmptyReceives int        `json:"empty_receives"`
	Problem       string     `json:"problem,omitempty"`
}

// targetResultV2 holds what differs from schema 3 beside the fields it shares.
// The outer Cells and Agentic shadow the embedded ones when decoding.
type targetResultV2 struct {
	targetResult

	Cells   []cellResultV2 `json:"cells"`
	Agentic agenticRun     `json:"agentic"`
}

type reportV2 struct {
	report

	Targets []targetResultV2 `json:"targets"`
}

// decodeV2 decodes a schema 2 file and converts it to schema 3.
func decodeV2(raw []byte) (report, error) {
	var old reportV2

	err := json.Unmarshal(raw, &old)
	if err != nil {
		return report{}, fmt.Errorf("schema 2: %w", err)
	}

	rep := old.report
	rep.Schema = reportSchema
	rep.Targets = make([]targetResult, 0, len(old.Targets))

	for _, ot := range old.Targets {
		t := ot.targetResult
		t.Cells = make([]cellResult, 0, len(ot.Cells))

		for _, oc := range ot.Cells {
			t.Cells = append(t.Cells, oc.convert())
		}

		if ot.Agentic.Cycles > 0 {
			t.Agentic = map[string]agenticRun{serviceSQS: ot.Agentic}
		}

		rep.Targets = append(rep.Targets, t)
	}

	return rep, nil
}

func (c cellResultV2) convert() cellResult {
	persistEnv := ""
	if c.Spec.Persist {
		persistEnv = persistEnvSQS
	}

	spec := sqsCellSpec(c.Spec.Payload, c.Spec.Queue, c.Spec.Shape, c.Spec.Proto, persistEnv, c.Spec.Concurrency)

	return cellResult{
		ID: spec.id(), Spec: spec, Runs: c.Runs,
		Ops:          map[string]opSpread{opSend: c.Send, opReceive: c.Receive, opDelete: c.Delete},
		CyclesPerSec: c.CyclesPerSec, PeakRSSBytes: c.PeakRSSBytes, CPUPercent: c.CPUPercent,
		Counters: map[string]int{counterEmptyReceives: c.EmptyReceives}, Problem: c.Problem,
	}
}
