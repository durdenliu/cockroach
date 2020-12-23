// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package rowexec

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/invertedexpr"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/invertedidx"
	"github.com/cockroachdb/cockroach/pkg/sql/row"
	"github.com/cockroachdb/cockroach/pkg/sql/rowcontainer"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/scrub"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/span"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/optional"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
)

// TODO(sumeer): adjust this batch size dynamically to balance between the
// higher scan throughput of larger batches and the cost of spilling the
// scanned rows to disk. The spilling cost will probably be dominated by
// the de-duping cost, since it incurs a read.
var invertedJoinerBatchSize = util.ConstantWithMetamorphicTestValue(
	100, /* defaultValue */
	1,   /* metamorphicValue */
)

// invertedJoinerState represents the state of the processor.
type invertedJoinerState int

const (
	ijStateUnknown invertedJoinerState = iota
	// ijReadingInput means that a batch of rows is being read from the input.
	ijReadingInput
	// ijPerformingIndexScan means it is performing an inverted index scan
	// for the current input row batch.
	ijPerformingIndexScan
	// ijEmittingRows means it is emitting the results of the inverted join.
	ijEmittingRows
)

type invertedJoiner struct {
	execinfra.ProcessorBase

	runningState invertedJoinerState
	diskMonitor  *mon.BytesMonitor
	desc         tabledesc.Immutable
	// The map from ColumnIDs in the table to the column position.
	colIdxMap catalog.TableColMap
	index     *descpb.IndexDescriptor
	// The ColumnID of the inverted column. Confusingly, this is also the id of
	// the table column that was indexed.
	invertedColID descpb.ColumnID

	onExprHelper execinfrapb.ExprHelper
	combinedRow  rowenc.EncDatumRow

	joinType descpb.JoinType

	// fetcher wraps the row.Fetcher used to perform scans. This enables the
	// invertedJoiner to wrap the fetcher with a stat collector when necessary.
	fetcher  rowFetcher
	alloc    rowenc.DatumAlloc
	rowAlloc rowenc.EncDatumRowAlloc

	// The row retrieved from the index represents the columns of the table
	// with the datums corresponding to the columns in the index populated.
	// The inverted column is in the position colIdxMap[invertedColID] and
	// the []byte stored there is used as the first parameter in
	// batchedExprEvaluator.addIndexRow(enc, keyIndex).
	//
	// The remaining columns in the index represent the primary key of the
	// table. They are at positions described by the keys in the
	// tableRowToKeyRowMap. The map is used to transform the retrieved table row
	// to the keyRow, and add to the row container, which de-duplicates the
	// primary keys. The index assigned by the container is the keyIndex in the
	// addIndexRow() call mentioned earlier.
	keyRow              rowenc.EncDatumRow
	keyTypes            []*types.T
	tableRowToKeyRowMap map[int]int
	// The reverse transformation, from a key row to a table row, is done
	// before evaluating the onExpr.
	tableRow            rowenc.EncDatumRow
	keyRowToTableRowMap []int

	// The input being joined using the index.
	input                execinfra.RowSource
	inputTypes           []*types.T
	datumsToInvertedExpr invertedexpr.DatumsToInvertedExpr
	canPreFilter         bool
	// Batch size for fetches. Not a constant so we can lower for testing.
	batchSize int

	// State variables for each batch of input rows.
	inputRows       rowenc.EncDatumRows
	batchedExprEval batchedInvertedExprEvaluator
	// The row indexes that are the result of the inverted expression evaluation
	// of the join. These will be further filtered using the onExpr.
	joinedRowIdx [][]KeyIndex

	// The container for the primary key rows retrieved from the index. For
	// evaluating each inverted expression, which involved set unions and
	// intersections, it is necessary to de-duplicate the primary key rows
	// retrieved from the inverted index. Instead of doing such de-duplication
	// for each expression in the batch of expressions, it is done once when
	// adding to keyRows -- this is more efficient since multiple expressions
	// may be using the same spans from the index.
	keyRows *rowcontainer.DiskBackedNumberedRowContainer

	// emitCursor contains information about where the next row to emit is within
	// joinedRowIdx.
	emitCursor struct {
		// inputRowIdx corresponds to joinedRowIdx[inputRowIdx].
		inputRowIdx int
		// outputRowIdx corresponds to joinedRowIdx[inputRowIdx][outputRowIdx].
		outputRowIdx int
		// seenMatch is true if there was a match at the current inputRowIdx.
		seenMatch bool
	}

	spanBuilder           *span.Builder
	outputContinuationCol bool
}

var _ execinfra.Processor = &invertedJoiner{}
var _ execinfra.RowSource = &invertedJoiner{}
var _ execinfrapb.MetadataSource = &invertedJoiner{}
var _ execinfra.OpNode = &invertedJoiner{}

const invertedJoinerProcName = "inverted joiner"

// newInvertedJoiner constructs an invertedJoiner. The datumsToInvertedExpr
// argument is non-nil only for tests. When nil, the invertedJoiner uses
// the spec to construct an implementation of DatumsToInvertedExpr.
func newInvertedJoiner(
	flowCtx *execinfra.FlowCtx,
	processorID int32,
	spec *execinfrapb.InvertedJoinerSpec,
	datumsToInvertedExpr invertedexpr.DatumsToInvertedExpr,
	input execinfra.RowSource,
	post *execinfrapb.PostProcessSpec,
	output execinfra.RowReceiver,
) (execinfra.RowSourcedProcessor, error) {
	switch spec.Type {
	case descpb.InnerJoin, descpb.LeftOuterJoin, descpb.LeftSemiJoin, descpb.LeftAntiJoin:
	default:
		return nil, errors.AssertionFailedf("unexpected inverted join type %s", spec.Type)
	}
	ij := &invertedJoiner{
		desc:                 tabledesc.MakeImmutable(spec.Table),
		input:                input,
		inputTypes:           input.OutputTypes(),
		datumsToInvertedExpr: datumsToInvertedExpr,
		joinType:             spec.Type,
		batchSize:            invertedJoinerBatchSize,
	}
	ij.colIdxMap = ij.desc.ColumnIdxMap()

	var err error
	ij.index, _, err = ij.desc.FindIndexByIndexIdx(int(spec.IndexIdx))
	if err != nil {
		return nil, err
	}
	ij.invertedColID = ij.index.ColumnIDs[0]

	indexColumnIDs, _ := ij.index.FullColumnIDs()
	// Inverted joins are not used for mutations.
	tableColumns := ij.desc.ColumnsWithMutations(false /* mutations */)
	ij.keyRow = make(rowenc.EncDatumRow, len(indexColumnIDs)-1)
	ij.keyTypes = make([]*types.T, len(ij.keyRow))
	ij.tableRow = make(rowenc.EncDatumRow, len(tableColumns))
	ij.tableRowToKeyRowMap = make(map[int]int)
	ij.keyRowToTableRowMap = make([]int, len(indexColumnIDs)-1)
	for i := 1; i < len(indexColumnIDs); i++ {
		keyRowIdx := i - 1
		tableRowIdx := ij.colIdxMap.GetDefault(indexColumnIDs[i])
		ij.tableRowToKeyRowMap[tableRowIdx] = keyRowIdx
		ij.keyRowToTableRowMap[keyRowIdx] = tableRowIdx
		ij.keyTypes[keyRowIdx] = ij.desc.Columns[tableRowIdx].Type
	}

	outputColCount := len(ij.inputTypes)
	// Inverted joins are not used for mutations.
	rightColTypes := ij.desc.ColumnTypesWithMutations(false /* mutations */)
	var includeRightCols bool
	if ij.joinType == descpb.InnerJoin || ij.joinType == descpb.LeftOuterJoin {
		outputColCount += len(rightColTypes)
		includeRightCols = true
		if spec.OutputGroupContinuationForLeftRow {
			outputColCount++
		}
	}
	outputColTypes := make([]*types.T, 0, outputColCount)
	outputColTypes = append(outputColTypes, ij.inputTypes...)
	if includeRightCols {
		outputColTypes = append(outputColTypes, rightColTypes...)
	}
	if spec.OutputGroupContinuationForLeftRow {
		outputColTypes = append(outputColTypes, types.Bool)
	}
	if err := ij.ProcessorBase.Init(
		ij, post, outputColTypes, flowCtx, processorID, output, nil, /* memMonitor */
		execinfra.ProcStateOpts{
			InputsToDrain: []execinfra.RowSource{ij.input},
			TrailingMetaCallback: func(ctx context.Context) []execinfrapb.ProducerMetadata {
				ij.close()
				return ij.generateMeta(ctx)
			},
		},
	); err != nil {
		return nil, err
	}

	semaCtx := flowCtx.TypeResolverFactory.NewSemaContext(flowCtx.EvalCtx.Txn)
	onExprColTypes := make([]*types.T, 0, len(ij.inputTypes)+len(rightColTypes))
	onExprColTypes = append(onExprColTypes, ij.inputTypes...)
	onExprColTypes = append(onExprColTypes, rightColTypes...)
	if err := ij.onExprHelper.Init(spec.OnExpr, onExprColTypes, semaCtx, ij.EvalCtx); err != nil {
		return nil, err
	}
	combinedRowLen := len(onExprColTypes)
	if spec.OutputGroupContinuationForLeftRow {
		combinedRowLen++
	}
	ij.combinedRow = make(rowenc.EncDatumRow, 0, combinedRowLen)

	if ij.datumsToInvertedExpr == nil {
		var invertedExprHelper execinfrapb.ExprHelper
		if err := invertedExprHelper.Init(spec.InvertedExpr, onExprColTypes, semaCtx, ij.EvalCtx); err != nil {
			return nil, err
		}
		ij.datumsToInvertedExpr, err = invertedidx.NewDatumsToInvertedExpr(
			ij.EvalCtx, onExprColTypes, invertedExprHelper.Expr, ij.index,
		)
		if err != nil {
			return nil, err
		}
	}
	ij.canPreFilter = ij.datumsToInvertedExpr.CanPreFilter()
	if ij.canPreFilter {
		ij.batchedExprEval.filterer = ij.datumsToInvertedExpr
	}

	var fetcher row.Fetcher
	// In general we need all the columns in the index to compute the set
	// expression. There may be InvertedJoinerSpec.InvertedExpr that are known
	// to generate only set union expressions, which together with LEFT_SEMI and
	// LEFT_ANTI, and knowledge of the columns needed by
	// InvertedJoinerSpec.OnExpr, could be used to prune the columns needed
	// here. For now, we do the simple thing, since we have no idea whether
	// such workloads actually occur in practice.
	allIndexCols := util.MakeFastIntSet()
	for _, colID := range indexColumnIDs {
		allIndexCols.Add(ij.colIdxMap.GetDefault(colID))
	}
	// We use ScanVisibilityPublic since inverted joins are not used for mutations,
	// and so do not need to see in-progress schema changes.
	_, _, err = initRowFetcher(
		flowCtx, &fetcher, &ij.desc, int(spec.IndexIdx), ij.colIdxMap, false, /* reverse */
		allIndexCols, false /* isCheck */, flowCtx.EvalCtx.Mon, &ij.alloc, execinfra.ScanVisibilityPublic,
		descpb.ScanLockingStrength_FOR_NONE, descpb.ScanLockingWaitPolicy_BLOCK,
		nil /* systemColumns */, nil, /* virtualColumn */
	)
	if err != nil {
		return nil, err
	}

	collectingStats := false
	if sp := tracing.SpanFromContext(flowCtx.EvalCtx.Ctx()); sp != nil && sp.IsVerbose() {
		collectingStats = true
	}
	if collectingStats {
		ij.input = newInputStatCollector(ij.input)
		ij.fetcher = newRowFetcherStatCollector(&fetcher)
		ij.ExecStatsForTrace = ij.execStatsForTrace
	} else {
		ij.fetcher = &fetcher
	}

	ij.spanBuilder = span.MakeBuilder(flowCtx.EvalCtx, flowCtx.Codec(), &ij.desc, ij.index)
	ij.spanBuilder.SetNeededColumns(allIndexCols)

	// Initialize memory monitors and row container for key rows.
	ctx := flowCtx.EvalCtx.Ctx()
	ij.MemMonitor = execinfra.NewLimitedMonitor(ctx, flowCtx.EvalCtx.Mon, flowCtx.Cfg, "invertedjoiner-limited")
	ij.diskMonitor = execinfra.NewMonitor(ctx, flowCtx.Cfg.DiskMonitor, "invertedjoiner-disk")
	ij.keyRows = rowcontainer.NewDiskBackedNumberedRowContainer(
		true, /* deDup */
		ij.keyTypes,
		ij.EvalCtx,
		ij.FlowCtx.Cfg.TempStorage,
		ij.MemMonitor,
		ij.diskMonitor,
	)

	ij.outputContinuationCol = spec.OutputGroupContinuationForLeftRow

	return ij, nil
}

// SetBatchSize sets the desired batch size. It should only be used in tests.
func (ij *invertedJoiner) SetBatchSize(batchSize int) {
	ij.batchSize = batchSize
}

// Next is part of the RowSource interface.
func (ij *invertedJoiner) Next() (rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
	// The join is implemented as follows:
	// - Read the input rows in batches.
	// - For each batch, map the rows to SpanExpressionProtos and initialize
	//   a batchedInvertedExprEvaluator. Use that evaluator to generate spans
	//   to read from the inverted index.
	// - Retrieve the index rows and add the primary keys in these rows to the
	//   row container, that de-duplicates, and pass the de-duplicated keys to
	//   the batch evaluator.
	// - Retrieve the results from the batch evaluator and buffer in joinedRowIdx,
	//   and use the emitCursor to emit rows.
	for ij.State == execinfra.StateRunning {
		var row rowenc.EncDatumRow
		var meta *execinfrapb.ProducerMetadata
		switch ij.runningState {
		case ijReadingInput:
			ij.runningState, meta = ij.readInput()
		case ijPerformingIndexScan:
			ij.runningState, meta = ij.performScan()
		case ijEmittingRows:
			ij.runningState, row, meta = ij.emitRow()
		default:
			log.Fatalf(ij.Ctx, "unsupported state: %d", ij.runningState)
		}
		if row == nil && meta == nil {
			continue
		}
		if meta != nil {
			return nil, meta
		}
		if outRow := ij.ProcessRowHelper(row); outRow != nil {
			return outRow, nil
		}
	}
	return nil, ij.DrainHelper()
}

// readInput reads the next batch of input rows and starts an index scan.
func (ij *invertedJoiner) readInput() (invertedJoinerState, *execinfrapb.ProducerMetadata) {
	// Read the next batch of input rows.
	for len(ij.inputRows) < ij.batchSize {
		row, meta := ij.input.Next()
		if meta != nil {
			if meta.Err != nil {
				ij.MoveToDraining(nil /* err */)
				return ijStateUnknown, meta
			}
			return ijReadingInput, meta
		}
		if row == nil {
			break
		}

		expr, preFilterState, err := ij.datumsToInvertedExpr.Convert(ij.Ctx, row)
		if err != nil {
			ij.MoveToDraining(err)
			return ijStateUnknown, ij.DrainHelper()
		}
		if expr == nil &&
			(ij.joinType != descpb.LeftOuterJoin && ij.joinType != descpb.LeftAntiJoin) {
			// One of the input columns was NULL, resulting in a nil expression.
			// The join type will emit no row since the evaluation result will be
			// an empty set, so don't bother copying the input row.
			ij.inputRows = append(ij.inputRows, nil)
		} else {
			ij.inputRows = append(ij.inputRows, ij.rowAlloc.CopyRow(row))
		}
		if expr == nil {
			// One of the input columns was NULL, resulting in a nil expression.
			// The nil serves as a marker that will result in an empty set as the
			// evaluation result.
			ij.batchedExprEval.exprs = append(ij.batchedExprEval.exprs, nil)
			if ij.canPreFilter {
				ij.batchedExprEval.preFilterState = append(ij.batchedExprEval.preFilterState, nil)
			}
		} else {
			ij.batchedExprEval.exprs = append(ij.batchedExprEval.exprs, expr)
			if ij.canPreFilter {
				ij.batchedExprEval.preFilterState = append(ij.batchedExprEval.preFilterState, preFilterState)
			}
		}
	}

	if len(ij.inputRows) == 0 {
		log.VEventf(ij.Ctx, 1, "no more input rows")
		// We're done.
		ij.MoveToDraining(nil)
		return ijStateUnknown, ij.DrainHelper()
	}
	log.VEventf(ij.Ctx, 1, "read %d input rows", len(ij.inputRows))

	spans := ij.batchedExprEval.init()
	if len(spans) == 0 {
		// Nothing to scan. For each input row, place a nil slice in the joined
		// rows, for emitRow() to process.
		ij.joinedRowIdx = ij.joinedRowIdx[:0]
		for range ij.inputRows {
			ij.joinedRowIdx = append(ij.joinedRowIdx, nil)
		}
		return ijEmittingRows, nil
	}
	// NB: spans is already sorted, and that sorting is preserved when
	// generating indexSpans.
	// TODO(mgartner): Pass a constraint that constrains the prefix columns of
	// multi-column inverted indexes.
	indexSpans, err := ij.spanBuilder.SpansFromInvertedSpans(spans, nil /* constraint */)
	if err != nil {
		ij.MoveToDraining(err)
		return ijStateUnknown, ij.DrainHelper()
	}

	log.VEventf(ij.Ctx, 1, "scanning %d spans", len(indexSpans))
	if err = ij.fetcher.StartScan(
		ij.Ctx, ij.FlowCtx.Txn, indexSpans, false /* limitBatches */, 0, /* limitHint */
		ij.FlowCtx.TraceKV); err != nil {
		ij.MoveToDraining(err)
		return ijStateUnknown, ij.DrainHelper()
	}

	return ijPerformingIndexScan, nil
}

func (ij *invertedJoiner) performScan() (invertedJoinerState, *execinfrapb.ProducerMetadata) {
	log.VEventf(ij.Ctx, 1, "joining rows")
	// Read the entire set of rows that are part of the scan.
	for {
		// Fetch the next row and copy it into the row container.
		scannedRow, _, _, err := ij.fetcher.NextRow(ij.Ctx)
		if err != nil {
			ij.MoveToDraining(scrub.UnwrapScrubError(err))
			return ijStateUnknown, ij.DrainHelper()
		}
		if scannedRow == nil {
			// Done with this input batch.
			break
		}

		// NB: Inverted columns are custom encoded in a manner that does not
		// correspond to Datum encoding, and in the code here we only want the
		// encoded bytes. Currently, we assume that the provider of this row has not
		// decoded the row, and therefore the encoded bytes can be used directly.
		// This will need to change if the rowFetcher used by the invertedJoiner is
		// changed to use to a vectorized implementation, however. In this case, the
		// fetcher will have decoded the row, but special-cased the inverted column
		// by stuffing the encoded bytes into a "decoded" DBytes. See
		// invertedFilterer.readInput() for an example.
		idx := ij.colIdxMap.GetDefault(ij.invertedColID)
		encInvertedVal := scannedRow[idx].EncodedBytes()
		shouldAdd, err := ij.batchedExprEval.prepareAddIndexRow(encInvertedVal)
		if err != nil {
			ij.MoveToDraining(err)
			return ijStateUnknown, ij.DrainHelper()
		}
		if shouldAdd {
			ij.transformToKeyRow(scannedRow)
			rowIdx, err := ij.keyRows.AddRow(ij.Ctx, ij.keyRow)
			if err != nil {
				ij.MoveToDraining(err)
				return ijStateUnknown, ij.DrainHelper()
			}
			if err = ij.batchedExprEval.addIndexRow(rowIdx); err != nil {
				ij.MoveToDraining(err)
				return ijStateUnknown, ij.DrainHelper()
			}
		}
	}
	ij.joinedRowIdx = ij.batchedExprEval.evaluate()
	ij.keyRows.SetupForRead(ij.Ctx, ij.joinedRowIdx)
	log.VEventf(ij.Ctx, 1, "done evaluating expressions")

	return ijEmittingRows, nil
}

var trueEncDatum = rowenc.DatumToEncDatum(types.Bool, tree.DBoolTrue)
var falseEncDatum = rowenc.DatumToEncDatum(types.Bool, tree.DBoolFalse)

// emitRow returns the next row from ij.emitCursor, if present. Otherwise it
// prepares for another input batch.
func (ij *invertedJoiner) emitRow() (
	invertedJoinerState,
	rowenc.EncDatumRow,
	*execinfrapb.ProducerMetadata,
) {
	// Finished processing the batch.
	if ij.emitCursor.inputRowIdx >= len(ij.joinedRowIdx) {
		log.VEventf(ij.Ctx, 1, "done emitting rows")
		// Ready for another input batch. Reset state.
		ij.inputRows = ij.inputRows[:0]
		ij.batchedExprEval.reset()
		ij.joinedRowIdx = nil
		ij.emitCursor.outputRowIdx = 0
		ij.emitCursor.inputRowIdx = 0
		ij.emitCursor.seenMatch = false
		if err := ij.keyRows.UnsafeReset(ij.Ctx); err != nil {
			ij.MoveToDraining(err)
			return ijStateUnknown, nil, ij.DrainHelper()
		}
		return ijReadingInput, nil, nil
	}

	// Reached the end of the matches for an input row. May need to emit for
	// LeftOuterJoin and LeftAntiJoin.
	if ij.emitCursor.outputRowIdx >= len(ij.joinedRowIdx[ij.emitCursor.inputRowIdx]) {
		inputRowIdx := ij.emitCursor.inputRowIdx
		seenMatch := ij.emitCursor.seenMatch
		ij.emitCursor.inputRowIdx++
		ij.emitCursor.outputRowIdx = 0
		ij.emitCursor.seenMatch = false

		if !seenMatch {
			switch ij.joinType {
			case descpb.LeftOuterJoin:
				ij.renderUnmatchedRow(ij.inputRows[inputRowIdx])
				return ijEmittingRows, ij.combinedRow, nil
			case descpb.LeftAntiJoin:
				return ijEmittingRows, ij.inputRows[inputRowIdx], nil
			}
		}
		return ijEmittingRows, nil, nil
	}

	inputRow := ij.inputRows[ij.emitCursor.inputRowIdx]
	joinedRowIdx := ij.joinedRowIdx[ij.emitCursor.inputRowIdx][ij.emitCursor.outputRowIdx]
	indexedRow, err := ij.keyRows.GetRow(ij.Ctx, joinedRowIdx, false /* skip */)
	if err != nil {
		ij.MoveToDraining(err)
		return ijStateUnknown, nil, ij.DrainHelper()
	}
	ij.emitCursor.outputRowIdx++
	ij.transformToTableRow(indexedRow)
	renderedRow, err := ij.render(inputRow, ij.tableRow)
	if err != nil {
		ij.MoveToDraining(err)
		return ijStateUnknown, nil, ij.DrainHelper()
	}
	skipRemaining := func() error {
		for ; ij.emitCursor.outputRowIdx < len(ij.joinedRowIdx[ij.emitCursor.inputRowIdx]); ij.emitCursor.outputRowIdx++ {
			idx := ij.joinedRowIdx[ij.emitCursor.inputRowIdx][ij.emitCursor.outputRowIdx]
			if _, err := ij.keyRows.GetRow(ij.Ctx, idx, true /* skip */); err != nil {
				return err
			}
		}
		return nil
	}
	if renderedRow != nil {
		seenMatch := ij.emitCursor.seenMatch
		ij.emitCursor.seenMatch = true
		switch ij.joinType {
		case descpb.InnerJoin, descpb.LeftOuterJoin:
			if ij.outputContinuationCol {
				if seenMatch {
					// This is not the first row output for this left row, so set the
					// group continuation to true.
					ij.combinedRow = append(ij.combinedRow, trueEncDatum)
				} else {
					// This is the first row output for this left row, so set the group
					// continuation to false.
					ij.combinedRow = append(ij.combinedRow, falseEncDatum)
				}
				renderedRow = ij.combinedRow
			}
			return ijEmittingRows, renderedRow, nil
		case descpb.LeftSemiJoin:
			// Skip the rest of the joined rows.
			if err := skipRemaining(); err != nil {
				ij.MoveToDraining(err)
				return ijStateUnknown, nil, ij.DrainHelper()
			}
			return ijEmittingRows, inputRow, nil
		case descpb.LeftAntiJoin:
			// Skip the rest of the joined rows.
			if err := skipRemaining(); err != nil {
				ij.MoveToDraining(err)
				return ijStateUnknown, nil, ij.DrainHelper()
			}
			ij.emitCursor.outputRowIdx = len(ij.joinedRowIdx[ij.emitCursor.inputRowIdx])
		}
	}
	return ijEmittingRows, nil, nil
}

// render constructs a row with columns from both sides. The ON condition is
// evaluated; if it fails, returns nil. When it returns a non-nil row, it is
// identical to ij.combinedRow.
func (ij *invertedJoiner) render(lrow, rrow rowenc.EncDatumRow) (rowenc.EncDatumRow, error) {
	ij.combinedRow = append(ij.combinedRow[:0], lrow...)
	ij.combinedRow = append(ij.combinedRow, rrow...)
	if ij.onExprHelper.Expr != nil {
		res, err := ij.onExprHelper.EvalFilter(ij.combinedRow)
		if !res || err != nil {
			return nil, err
		}
	}
	return ij.combinedRow, nil
}

// renderUnmatchedRow creates a result row given an unmatched row and
// stores it in ij.combinedRow.
func (ij *invertedJoiner) renderUnmatchedRow(row rowenc.EncDatumRow) {
	ij.combinedRow = ij.combinedRow[:cap(ij.combinedRow)]
	// Copy the left row.
	copy(ij.combinedRow, row)
	// Set the remaining columns to NULL.
	for i := len(row); i < len(ij.combinedRow); i++ {
		ij.combinedRow[i].Datum = tree.DNull
	}
	if ij.outputContinuationCol {
		// The last column is the continuation column, so set it to false since
		// this is the only output row for this group.
		ij.combinedRow[len(ij.combinedRow)-1] = falseEncDatum
	}
}

func (ij *invertedJoiner) transformToKeyRow(row rowenc.EncDatumRow) {
	for i, rowIdx := range ij.keyRowToTableRowMap {
		ij.keyRow[i] = row[rowIdx]
	}
}

func (ij *invertedJoiner) transformToTableRow(keyRow rowenc.EncDatumRow) {
	for r, k := range ij.tableRowToKeyRowMap {
		ij.tableRow[r] = keyRow[k]
	}
}

// Start is part of the RowSource interface.
func (ij *invertedJoiner) Start(ctx context.Context) context.Context {
	ij.input.Start(ctx)
	ctx = ij.StartInternal(ctx, invertedJoinerProcName)
	ij.runningState = ijReadingInput
	return ctx
}

// ConsumerClosed is part of the RowSource interface.
func (ij *invertedJoiner) ConsumerClosed() {
	// The consumer is done, Next() will not be called again.
	ij.close()
}

func (ij *invertedJoiner) close() {
	if ij.InternalClose() {
		if ij.fetcher != nil {
			ij.fetcher.Close(ij.Ctx)
		}
		if ij.keyRows != nil {
			ij.keyRows.Close(ij.Ctx)
		}
		ij.MemMonitor.Stop(ij.Ctx)
		if ij.diskMonitor != nil {
			ij.diskMonitor.Stop(ij.Ctx)
		}
	}
}

// execStatsForTrace implements ProcessorBase.ExecStatsForTrace.
func (ij *invertedJoiner) execStatsForTrace() *execinfrapb.ComponentStats {
	is, ok := getInputStats(ij.input)
	if !ok {
		return nil
	}
	fis, ok := getFetcherInputStats(ij.fetcher)
	if !ok {
		return nil
	}
	return &execinfrapb.ComponentStats{
		Inputs: []execinfrapb.InputStats{is},
		KV: execinfrapb.KVStats{
			TuplesRead:     fis.NumTuples,
			KVTime:         fis.WaitTime,
			ContentionTime: optional.MakeTimeValue(getCumulativeContentionTime(ij.fetcher.GetContentionEvents())),
		},
		Exec: execinfrapb.ExecStats{
			MaxAllocatedMem:  optional.MakeUint(uint64(ij.MemMonitor.MaximumBytes())),
			MaxAllocatedDisk: optional.MakeUint(uint64(ij.diskMonitor.MaximumBytes())),
		},
		Output: ij.Out.Stats(),
	}
}

func (ij *invertedJoiner) generateMeta(ctx context.Context) []execinfrapb.ProducerMetadata {
	var trailingMeta []execinfrapb.ProducerMetadata
	if tfs := execinfra.GetLeafTxnFinalState(ctx, ij.FlowCtx.Txn); tfs != nil {
		trailingMeta = append(trailingMeta, execinfrapb.ProducerMetadata{LeafTxnFinalState: tfs})
	}
	if contentionEvents := ij.fetcher.GetContentionEvents(); len(contentionEvents) != 0 {
		trailingMeta = append(trailingMeta, execinfrapb.ProducerMetadata{ContentionEvents: contentionEvents})
	}
	return trailingMeta
}

// DrainMeta is part of the MetadataSource interface.
func (ij *invertedJoiner) DrainMeta(ctx context.Context) []execinfrapb.ProducerMetadata {
	return ij.generateMeta(ctx)
}

// ChildCount is part of the execinfra.OpNode interface.
func (ij *invertedJoiner) ChildCount(verbose bool) int {
	if _, ok := ij.input.(execinfra.OpNode); ok {
		return 1
	}
	return 0
}

// Child is part of the execinfra.OpNode interface.
func (ij *invertedJoiner) Child(nth int, verbose bool) execinfra.OpNode {
	if nth == 0 {
		if n, ok := ij.input.(execinfra.OpNode); ok {
			return n
		}
		panic("input to invertedJoiner is not an execinfra.OpNode")
	}
	panic(errors.AssertionFailedf("invalid index %d", nth))
}
