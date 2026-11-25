package dynamodb_test

import "math"

// The one predicate that cuts a cell out of a DynamoDB benchmark matrix
// (docs/standards.md, "Benchmarks"). One iteration of a cell carries some load,
// counted in the unit of its family: bytes of items, actions, results, units of
// read cost. A family names its budget, the most load an iteration may carry so
// that the slowest of the cell's runs on the gate's runners is at most 8 ms,
// and the comment above the budget says what it leaves out and keeps with the
// time each took. The budgets, by file:
//
//   - batch_benchmark_test.go: maxIterationBatchBytes, maxIterationWriteBytes,
//     maxIterationActions and maxIterationWriteActions;
//   - items_benchmark_test.go: maxIterationPutBytes;
//   - reads_benchmark_test.go: maxIterationReadItems and maxIterationReadBytes,
//     the second also for the unkeyed reads of BenchmarkPartiQLSelect;
//   - partiql_benchmark_test.go: selectWidthBudget, maxNarrowResults and
//     maxUpdateWidthActions;
//   - packed_benchmark_test.go: maxItemReadUnits;
//   - decode-budget_benchmark_test.go: maxIterationDecodeBytes;
//   - ttl_benchmark_test.go: maxExpiredAllItems.
//
// A family that keeps every cell passes unbudgeted. The counts that a ladder
// cuts (snapshotCases, restoreCases) are lists, not budgets.

// unbudgeted is the budget of a family none of whose cells is over the limit.
const unbudgeted = math.MaxInt

// tooSlowToGate reports whether a cell whose iteration carries load is over
// budget, and so is left out of its matrix. A cell that is left out is not a
// missing row: the axis is covered by the widest cell under the budget.
func tooSlowToGate(load, budget int) bool {
	return load > budget
}
